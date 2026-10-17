package device

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/yibaiba/hideck/internal/modemvoice/audio"
	"github.com/yibaiba/hideck/internal/modemvoice/host"
	"github.com/yibaiba/hideck/internal/modemvoice/media"
)

// 原生声卡路线不设固件白名单：除已验证的 EC20CEFAGR08A03M4G 外，其他 EC20
// 固件版本、以及其他暴露原生 USB 声卡的 Quectel 模组，只要声卡在，就走同一条
// 链路——QPCMV 尽力而为，失败即本次媒体路径失败，不建立无声通话。这类模组没有
// QDC507 的 Android 运行时，因此整条流程不碰 ADB、insmod、脚本推送。
type nativeVoiceOwner struct{}

func (nativeVoiceOwner) Close() error { return nil }

// prepareNativeVoice 构建模组原生 USB 声卡的直拨音频路由，与原生 VoLTE 共用
// 同一条硬件链路：模组 USB 声卡（运行时按 IMEI 解析）+ arecord/aplay。
// 准备期先确认声卡存在：解析不到配对采集/播放节点就直接拒绝，别等到通话才失败。
func prepareNativeVoice(ctx context.Context, port *modemVoicePort) (*host.Resources, error) {
	capture, err := exec.LookPath("arecord")
	if err != nil {
		return nil, errors.New("未找到 arecord，请安装 alsa-utils 后重试")
	}
	playback, err := exec.LookPath("aplay")
	if err != nil {
		return nil, errors.New("未找到 aplay，请安装 alsa-utils 后重试")
	}
	usbPath := port.worker.Config.USBPath
	// 黑名单先行：DJI/百网 EC25 复合设备的 UAC 能枚举但一开就切 altsetting、
	// 杀死同设备的 QMI，这种声卡宁可拒绝也不碰。
	if modemUACUnusable(usbPath) {
		return nil, errors.New("模组 USB 声卡不可用（打开会破坏 QMI），拒绝原生音频")
	}
	if _, err := audio.ResolveUSBPCM(usbPath); err != nil {
		return nil, fmt.Errorf("未找到模组原生 USB 声卡，无法走原生音频: %w", err)
	}
	alsa := audio.ALSA{CaptureProgram: capture, PlaybackProgram: playback}
	return &host.Resources{
		Port:    port,
		Check:   port.checkIdentity,
		CanCall: port.check,
		// 无后台运行时可拆卸；QPCMV 随模组重启自然失效，下次接通补发。
		Close: nil,
		Route: func() (media.AudioRoute, error) {
			// RuntimeRoute 一次性使用，每次通话取一条新路由。
			return audio.NewRuntimeRoute(audio.RuntimeRouteOptions{
				Start: func(ctx context.Context) (io.Closer, error) {
					// 把通话 PCM 接到模组 USB 声卡。失败即媒体路径失败，
					// 不建立无声通话。
					if _, err := port.ExecuteATContext(ctx, "AT+QPCMV=1,2", 2*time.Second); err != nil {
						return nil, fmt.Errorf("模组无法把通话 PCM 接到 USB 声卡: %w", err)
					}
					return nativeVoiceOwner{}, nil
				},
				Resolve: func() (audio.Endpoint, error) {
					// 声卡号随枚举变化，每次现解析，与 QDC507 路径同一约定。
					return audio.ResolveUSBPCM(usbPath)
				},
				OpenPCM: func(ctx context.Context, endpoint audio.Endpoint) (media.PCM, error) {
					return alsa.Open(ctx, endpoint)
				},
			})
		},
	}, nil
}
