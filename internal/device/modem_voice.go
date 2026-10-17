package device

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/yibaiba/hideck/internal/modemvoice/audio"
	"github.com/yibaiba/hideck/internal/modemvoice/host"
	"github.com/yibaiba/hideck/internal/modemvoice/media"
	"github.com/yibaiba/hideck/internal/modemvoice/qdc507"
	"github.com/yibaiba/hideck/pkg/logger"
)

func (p *Pool) ModemVoiceController() *host.Controller { return p.modemVoiceCtl }

func (p *Pool) ModemVoiceStatus(id string) map[string]interface{} {
	if p.modemVoiceCtl == nil || !p.IsModemVoice(id) {
		return nil
	}
	return p.modemVoiceCtl.DeviceStatus(id)
}

func (p *Pool) IsModemVoice(id string) bool {
	w := p.GetWorker(id)
	return w != nil && IsModemVoiceMode(w.Config.PhoneMode) && PhoneServiceEnabled(w.Config)
}

func (p *Pool) newModemVoiceController() *host.Controller {
	return host.New(host.Options{Prepare: p.prepareModemVoice, Identity: func(id string) string {
		w := p.GetWorker(id)
		if w == nil {
			return ""
		}
		sim := currentSIMOwnership(w)
		return fmt.Sprintf("%p:%s:%d", w, sim.iccid, sim.generation)
	}, Listen: func() (net.PacketConn, error) {
		return net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	}})
}

func (p *Pool) stopModemVoice(id string) error {
	if p.modemVoiceCtl == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	err := p.modemVoiceCtl.Disable(ctx, id)
	if err != nil {
		logger.Warn("模组直拨停止失败", "device", id, "err", err)
	}
	return err
}

func (p *Pool) reconcileModemVoice(w *Worker) error {
	if p.modemVoiceCtl == nil {
		return nil
	}
	if !p.IsModemVoice(w.ID) || w.Config.AirplaneEnabled {
		return p.stopModemVoice(w.ID)
	}
	if err := p.stopNativeVoLTEForModemVoice(w.ID); err != nil {
		return err
	}
	if err := p.StopSoftwareIMS(w.ID); err != nil {
		return err
	}
	p.modemVoiceCtl.Enable(p.Context(), w.ID)
	return nil
}

func (p *Pool) stopNativeVoLTEForModemVoice(id string) error {
	p.cancelNativeVoLTEStart(id)
	if p.volteCtl == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(p.Context(), 90*time.Second)
	defer cancel()
	return p.volteCtl.DisableForHandoff(ctx, id)
}

// This constructor is the infrastructure boundary. Optional tools and bundles
// are accessed only after the user explicitly selects modem_voice.
func (p *Pool) prepareModemVoice(ctx context.Context, id string) (*host.Resources, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("模组直拨音频目前仅支持 Linux")
	}
	w := p.GetWorker(id)
	if w == nil || w.CurrentICCID() == "" {
		return nil, errors.New("模组直拨需要已识别的 SIM 卡")
	}
	sim := currentSIMOwnership(w)
	port := &modemVoicePort{pool: p, worker: w, iccid: sim.iccid, identityGeneration: sim.generation}
	if err := port.check(ctx); err != nil {
		return nil, err
	}
	// 型号先行：ATI 一条指令同时给出厂商/型号/固件（Revision 行），认不出
	// QDC507 就绝不进入 QDC 的 ADB/insmod 初始化流程。
	identity, err := port.ExecuteATContext(ctx, "ATI", 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("读取模组型号失败: %w", err)
	}
	model, firmware := parseATI(identity)
	logger.Info("模组直拨音频适配器", "device", id, "adapter", string(modemVoiceAdapter(identity)),
		"model", model, "firmware", firmware)
	switch modemVoiceAdapter(identity) {
	case modemVoiceAdapterQDC507:
		// QDC 流程按验证过的固件跑，ATI 的 Revision 行与 CGMR 同源，仍以
		// CGMR 复核一次，避免把 ADB/insmod 序列跑到未验证固件上。
		revision, err := port.ExecuteATContext(ctx, "AT+CGMR", 5*time.Second)
		if err != nil {
			return nil, fmt.Errorf("读取模组固件失败: %w", err)
		}
		if !hasFirmware(revision, qdc507VoiceFirmware) {
			return nil, errors.New("QDC507 模组固件未验证，拒绝模组直拨初始化")
		}
		return prepareQDC507Voice(ctx, port)
	case modemVoiceAdapterNative:
		return prepareNativeVoice(ctx, port)
	default:
		return nil, errors.New("当前模组没有已验证的模组直拨音频适配器")
	}
}

// modemVoiceAdapterKind 选择模组直拨的音频适配器。空值=拒绝初始化。
type modemVoiceAdapterKind string

const (
	modemVoiceAdapterNone   modemVoiceAdapterKind = ""
	modemVoiceAdapterQDC507 modemVoiceAdapterKind = "qdc507"
	modemVoiceAdapterNative modemVoiceAdapterKind = "native"
)

// modemVoiceAdapter 只认 ATI：响应里出现 QDC507 才可能是 QDC 模组（其固件
// 由调用方以 CGMR 严格复核）；其余一切型号（EC20 全固件版本、其他暴露原生
// USB 声卡的模组）走原生声卡流程，由准备期声卡存在性检查兜底。
func modemVoiceAdapter(identity string) modemVoiceAdapterKind {
	if strings.Contains(strings.ToUpper(identity), "QDC507") {
		return modemVoiceAdapterQDC507
	}
	return modemVoiceAdapterNative
}

// parseATI 解析 ATI 响应，例如：
//
//	Quectel
//	EC20F
//	Revision: EC20CEFAGR08A03M4G
//	OK
//
// 型号取 Revision 行前的最后一条非空行（单行型号也兼容），固件取
// Revision 值；没有 Revision 行时把全部非空行拼成型号。
func parseATI(response string) (model, firmware string) {
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(response, "\r", ""), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" && trimmed != "OK" {
			lines = append(lines, trimmed)
		}
	}
	for i, line := range lines {
		if strings.HasPrefix(strings.ToLower(line), "revision:") {
			firmware = strings.TrimSpace(line[len("revision:"):])
			if i > 0 {
				model = lines[i-1]
			}
			return model, firmware
		}
	}
	return strings.Join(lines, " "), ""
}

// qdc507VoiceFirmware 是已验证的 QDC507 模组直拨音频适配器固件版本。
const qdc507VoiceFirmware = "QDC507GLEFM21"

func prepareQDC507Voice(ctx context.Context, port *modemVoicePort) (*host.Resources, error) {
	adb, err := qdc507.FindADB()
	if err != nil {
		return nil, err
	}
	capture, err := exec.LookPath("arecord")
	if err != nil {
		return nil, errors.New("未找到 arecord，请安装 alsa-utils 后重试")
	}
	playback, err := exec.LookPath("aplay")
	if err != nil {
		return nil, errors.New("未找到 aplay，请安装 alsa-utils 后重试")
	}
	root, err := filepath.Abs(filepath.Join("data", "modem-voice"))
	if err != nil {
		return nil, err
	}
	bundle, err := qdc507.EmbeddedBundle()
	if err != nil {
		return nil, err
	}
	return prepareQDC507(ctx, port, qdc507Setup{adb: adb, state: filepath.Join(root, "adb"),
		bundle: bundle, capture: capture, playback: playback})
}

type qdc507Setup struct {
	adb, state, capture, playback string
	bundle                        fs.FS
}

func prepareQDC507(ctx context.Context, port *modemVoicePort, setup qdc507Setup) (*host.Resources, error) {
	executor, err := qdc507.NewExec(qdc507.ExecOptions{Program: setup.adb, StateDirectory: setup.state})
	if err != nil {
		return nil, err
	}
	client, err := qdc507.NewADB(executor)
	if err != nil {
		return nil, err
	}
	if err := port.prepareADB(ctx, client, setup.state); err != nil {
		return nil, err
	}
	manager, err := qdc507.NewManager(qdc507.Options{USB: filepath.Base(port.worker.Config.USBPath),
		Firmware: "QDC507GLEFM21", Client: client, Source: setup.bundle, Check: port.check})
	if err != nil {
		return nil, err
	}
	resources := &host.Resources{Port: port, Check: port.checkIdentity, CanCall: port.check, Close: manager.Shutdown,
		Route: nil}
	alsa := audio.ALSA{CaptureProgram: setup.capture, PlaybackProgram: setup.playback}
	resources.Route = func() (media.AudioRoute, error) {
		return manager.AudioRoute(qdc507.AudioOptions{Resolve: func() (audio.Endpoint, error) {
			return audio.ResolveUSBPCM(port.worker.Config.USBPath)
		}, OpenPCM: alsa.Open})
	}
	_, err = manager.Prepare(ctx)
	return resources, err
}

func hasFirmware(response, firmware string) bool {
	for _, line := range strings.Split(strings.ReplaceAll(response, "\r", ""), "\n") {
		if strings.TrimSpace(line) == firmware {
			return true
		}
	}
	return false
}
