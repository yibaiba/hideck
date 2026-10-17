//go:build linux

package volte

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/yibaiba/hideck/pkg/logger"
)

const (
	alsaIoctlHWRefine = 0xc2604110
	alsaIoctlHWParams = 0xc2604111
	alsaIoctlPrepare  = 0x4140
	alsaIoctlDrop     = 0x4143
)

// 播放缓冲从 4 帧（80ms）放大到 8 帧（160ms）。
// EC25 语音 UAC 只支持 8000Hz/单声道，浏览器 RTP 有天然抖动，
// 缓冲越厚越不容易欠载；internal/modemvoice/audio 的播放队列用的
// 就是 200ms 级别的缓冲经验值。
const alsaPlayBufferFrames = 8

// alsaPlayXRUN / alsaPlayRecovered / alsaPlayUnrecovered 供
// PCMBridge 带进日志，避免"模组侧静音但无人知晓"。
var (
	alsaPlayXRUN         atomic.Uint64
	alsaPlayRecovered    atomic.Uint64
	alsaPlayUnrecovered  atomic.Uint64
)

type alsaStream struct {
	f *os.File
}

type alsaPCM struct {
	device string
	play   alsaStream
	capt   alsaStream
	mu     sync.Mutex
}

func openALSAPCM(device string) (PCMPort, error) {
	card, pcmDev, err := parseALSADevice(device)
	if err != nil {
		return nil, err
	}
	// 先激活再打开正式流：模组冷启动（重启/USB 重新枚举）后 USB 等时端点
	// 停在 idle，此时直接实时写会从第二帧起全部 -EPIPE，上行全程静默。
	activatePlaybackEndpoint(device)
	play, err := openALSAStream(fmt.Sprintf("/dev/snd/pcmC%dD%dp", card, pcmDev), true)
	if err != nil {
		return nil, err
	}
	capt, err := openALSAStream(fmt.Sprintf("/dev/snd/pcmC%dD%dc", card, pcmDev), false)
	if err != nil {
		_ = play.close()
		return nil, err
	}
	return &alsaPCM{device: device, play: play, capt: capt}, nil
}

func openALSAStream(path string, playback bool) (alsaStream, error) {
	flag := unix.O_RDONLY
	if playback {
		flag = unix.O_WRONLY
	}
	f, err := os.OpenFile(path, flag, 0)
	if err != nil {
		return alsaStream{}, fmt.Errorf("volte: open %s: %w", path, err)
	}
	if err := configureALSA(f); err != nil {
		_ = f.Close()
		return alsaStream{}, fmt.Errorf("volte: configure %s: %w", path, err)
	}
	return alsaStream{f: f}, nil
}

func configureALSA(f *os.File) error {
	params := newALSAHWParams()
	constrainALSAMask(&params, alsaParamAccess, alsaAccessRWInter)
	constrainALSAMask(&params, alsaParamFormat, alsaFormatS16LE)
	constrainALSAInterval(&params, alsaParamChannels, 1)
	constrainALSAInterval(&params, alsaParamRate, uint32(pcmuClockRate))
	constrainALSAInterval(&params, alsaParamPeriodSz, uint32(pcmuFrameSamples))
	constrainALSAInterval(&params, alsaParamBufferSz, uint32(pcmuFrameSamples*alsaPlayBufferFrames))
	if err := ioctl(f, alsaIoctlHWRefine, unsafe.Pointer(&params[0])); err != nil {
		return err
	}
	if err := ioctl(f, alsaIoctlHWParams, unsafe.Pointer(&params[0])); err != nil {
		return err
	}
	return ioctl(f, alsaIoctlPrepare, nil)
}

func ioctl(f *os.File, req uint, arg unsafe.Pointer) error {
	var ap uintptr
	if arg != nil {
		ap = uintptr(arg)
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), uintptr(req), ap)
	if errno != 0 {
		return errno
	}
	return nil
}

// alsaPlayWarmupFrames 是端点激活时写入的静音量，4 帧 = 80ms。
const alsaPlayWarmupFrames = 4

// alsaPlayWarmupTimeout 限制单次端点激活的等待时间。实测空闲时 aplay 只要
// 275ms，但上一通刚结束时设备还没释放，aplay 会卡在等设备；给 1.2s 余量，
// 同时靠 alsaPlayWarmupAttempts 重试，避免一上来就判死。
const alsaPlayWarmupTimeout = 1200 * time.Millisecond

// alsaPlayWarmupAttempts 是激活重试次数：第一次可能撞上上一通的设备释放窗口。
const alsaPlayWarmupAttempts = 2

// activatePlaybackEndpoint 用 aplay 跑一次完整的 open → write → drain →
// close 周期，把模组冷启动后停在 idle 的 USB 等时端点拉到 streaming。
//
// 为什么不用自己写 raw ioctl：实测只做 HW_PARAMS + PREPARE + 写满缓冲 +
// DRAIN 仍然救不回来（随后实时写每帧 -EPIPE、alsa_recovered 恒为 0），而
// 手动执行一次 aplay 后同一通路立刻正常（to_modem 由 1 变 394、XRUN 只
// 剩首帧一次）。差别在 aplay 还会设置 sw_params（start_threshold 等）并在
// 退出时 hw_free —— 这些细节不值得在业务代码里对着内核协议猜，直接复用
// 模组直拨路线已经在用的那条命令。
func activatePlaybackEndpoint(device string) {
	aplay, err := exec.LookPath("aplay")
	if err != nil {
		logger.Warn("VoLTE 声卡端点激活跳过：未找到 aplay", "device", device)
		return
	}
	start := time.Now()
	var lastErr error
	var lastOut string
	for attempt := 1; attempt <= alsaPlayWarmupAttempts; attempt++ {
		err, out := runPlaybackWarmup(aplay, device)
		if err == nil {
			logger.Info("VoLTE 声卡端点已激活", "device", device,
				"attempt", attempt, "ms", time.Since(start).Milliseconds())
			return
		}
		lastErr, lastOut = err, out
		if attempt < alsaPlayWarmupAttempts {
			// 上一通的设备句柄可能还没释放，稍等再试。
			time.Sleep(150 * time.Millisecond)
		}
	}
	logger.Warn("VoLTE 声卡端点激活失败，本次通话可能没有上行",
		"device", device, "err", lastErr, "out", lastOut)
}

func runPlaybackWarmup(aplay, device string) (error, string) {
	ctx, cancel := context.WithTimeout(context.Background(), alsaPlayWarmupTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, aplay, "-D", device, "-f", "S16_LE",
		"-r", "8000", "-c", "1", "-q")
	cmd.Stdin = bytes.NewReader(make([]byte, pcmuFrameSamples*2*alsaPlayWarmupFrames))
	out, err := cmd.CombinedOutput()
	return err, strings.TrimSpace(string(out))
}

func (p *alsaPCM) ReadFrame() ([]int16, error) {
	capt := p.captureFile()
	if capt == nil {
		return nil, fmt.Errorf("volte: capture closed")
	}
	buf := make([]byte, pcmuFrameSamples*2)
	if _, err := capt.Read(buf); err != nil {
		return nil, err
	}
	out := make([]int16, pcmuFrameSamples)
	for i := 0; i < pcmuFrameSamples; i++ {
		out[i] = int16(buf[i*2]) | int16(buf[i*2+1])<<8
	}
	return out, nil
}

func (p *alsaPCM) WriteFrame(samples []int16) error {
	play := p.playbackFile()
	if play == nil {
		return nil
	}
	buf := make([]byte, pcmuFrameSamples*2)
	n := len(samples)
	if n > pcmuFrameSamples {
		n = pcmuFrameSamples
	}
	for i := 0; i < n; i++ {
		v := samples[i]
		buf[i*2] = byte(v)
		buf[i*2+1] = byte(v >> 8)
	}
	// ALSA 阻塞写失败几乎只有一种原因：XRUN。流已启动但缓冲区被抽空，
	// write() 返回 -EPIPE，此后不做 PREPARE 恢复的话，后续每一帧都会失败，
	// 对方全程静默；capture 是独立数据流不受影响，表现就是
	// "我听得到对方、对方听不到我"。
	// 这里恢复一次：DROP 丢弃残留 → PREPARE 重启流 → 补一帧静音把空缓冲
	// 垫起来 → 重写本帧。思路与 internal/modemvoice/audio 的播放队列一致。
	for attempt := 0; ; attempt++ {
		_, err := play.Write(buf)
		if err == nil {
			if attempt > 0 {
				alsaPlayRecovered.Add(1)
			}
			return nil
		}
		if attempt > 0 || !isALSAXrun(err) {
			alsaPlayUnrecovered.Add(1)
			return err
		}
		alsaPlayXRUN.Add(1)
		if ioctl(play, alsaIoctlDrop, nil) != nil || ioctl(play, alsaIoctlPrepare, nil) != nil {
			alsaPlayUnrecovered.Add(1)
			return err
		}
		// 补一帧静音，避免恢复瞬间缓冲仍空、下一帧再次欠载。
		// 注意：这里只算"恢复动作发起成功"，recovered 统一由下面重写
		// 成功时计数，否则一次恢复会被算成两次。
		if _, perr := play.Write(make([]byte, pcmuFrameSamples*2)); perr != nil {
			alsaPlayUnrecovered.Add(1)
			return err
		}
	}
}

// isALSAXrun 判断写播放流失败是否为欠载。注意 os.File.Write 会把
// errno 包进 *fs.PathError（"write /dev/snd/pcmC3D0p: broken pipe"），
// 必须用 errors.Is 解包，直接类型断言 unix.Errno 会漏掉全部 XRUN。
func isALSAXrun(err error) bool {
	return errors.Is(err, unix.EPIPE) || errors.Is(err, unix.ESTRPIPE)
}

func (p *alsaPCM) captureFile() *os.File {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.capt.f
}

func (p *alsaPCM) playbackFile() *os.File {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.play.f
}

func (p *alsaPCM) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	play, capt := p.play.f, p.capt.f
	p.play.f, p.capt.f = nil, nil
	p.mu.Unlock()
	closeALSAFile(play)
	closeALSAFile(capt)
	return nil
}

const alsaCloseBudget = 300 * time.Millisecond

func closeALSAFile(f *os.File) {
	if f == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		_ = ioctl(f, alsaIoctlDrop, nil)
		_ = f.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(alsaCloseBudget):
		_ = f.Close()
	}
}

func (s alsaStream) close() error {
	if s.f == nil {
		return nil
	}
	return s.f.Close()
}
