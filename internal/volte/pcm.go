package volte

import (
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/yibaiba/hideck/internal/phone"
	"github.com/yibaiba/hideck/pkg/logger"
)

const (
	pcmuPayloadType   = 0
	pcmuClockRate     = 8000
	pcmuFrameSamples  = 160
	pcmuFrameDuration = 20 * time.Millisecond
)

type PCMPort interface {
	ReadFrame() ([]int16, error)
	WriteFrame([]int16) error
	Close() error
}

type pcmDiagnostics struct {
	XRUN        uint64
	Recovered   uint64
	Unrecovered uint64
}

type pcmDiagnosticsProvider interface {
	pcmDiagnostics() pcmDiagnostics
}

func diagnosticsForPCM(pcm PCMPort) pcmDiagnostics {
	provider, ok := pcm.(pcmDiagnosticsProvider)
	if !ok {
		return pcmDiagnostics{}
	}
	return provider.pcmDiagnostics()
}

type PCMBridge struct {
	conn       net.PacketConn
	mediaMu    sync.Mutex
	remote     net.Addr
	generation uint64
	buffer     *adaptivePCMBuffer
	pcm        PCMPort
	listenOnly bool
	closed     chan struct{}
	closeOnce  sync.Once
	workers    sync.WaitGroup
	seq        uint16
	ts         uint32
	toPCM      atomic.Uint64
	fromPCM    atomic.Uint64
	silent     atomic.Uint64
	lost       atomic.Uint64
	overflow   atomic.Uint64
	jitterMu   sync.Mutex
	jitter     pcmJitterDiagnostics
}

type pcmRTPArrival struct {
	packet     *rtp.Packet
	remote     net.Addr
	received   time.Time
	generation uint64
}

type pcmWriteLogState struct {
	overflowLogged bool
	lastOverflow   time.Time
}

func NewPCMBridge(conn net.PacketConn, remote net.Addr, pcm PCMPort, listenOnly bool) *PCMBridge {
	b := &PCMBridge{conn: conn, remote: remote, pcm: pcm, listenOnly: listenOnly, closed: make(chan struct{}), buffer: newAdaptivePCMBuffer()}
	b.workers.Add(2)
	go b.downlink()
	go b.uplink()
	return b
}

func (b *PCMBridge) Stats() (toPCM, fromPCM, silent, lost, overflow uint64) {
	return b.toPCM.Load(), b.fromPCM.Load(), b.silent.Load(), b.lost.Load(), b.overflow.Load()
}

func (b *PCMBridge) Close() error {
	var err error
	b.closeOnce.Do(func() {
		close(b.closed)
		if b.conn != nil {
			err = b.conn.Close()
		}
		if b.pcm != nil {
			_ = b.pcm.Close()
		}
		b.workers.Wait()
		diagnostics := diagnosticsForPCM(b.pcm)
		jitter := b.jitterStats()
		// 挂断即打模组侧统计：toPCM/overflow 才是"音频是否真的进了模组"的
		// 证据，phone 包的 to_modem 只是回环 UDP 写成功计数，不等于送达模组。
		logger.Info("VoLTE 模组音频统计",
			"to_modem", b.toPCM.Load(), "from_modem", b.fromPCM.Load(),
			"silent", b.silent.Load(), "lost", b.lost.Load(), "overflow", b.overflow.Load(),
			"alsa_xrun", diagnostics.XRUN, "alsa_recovered", diagnostics.Recovered,
			"alsa_unrecovered", diagnostics.Unrecovered,
			"jitter_target_ms", jitter.TargetFrames*uint64(pcmuFrameDuration/time.Millisecond),
			"jitter_max_depth", jitter.MaxDepth,
			"jitter_us", jitter.JitterMicros, "jitter_received", jitter.Received,
			"jitter_late", jitter.Late, "jitter_duplicate", jitter.Duplicate,
			"jitter_concealed", jitter.Concealed, "jitter_trimmed", jitter.Trimmed,
			"jitter_rebuffers", jitter.Rebuffers, "jitter_restarts", jitter.Restarts)
	})
	return err
}

func (b *PCMBridge) downlink() {
	defer b.workers.Done()
	arrivals := make(chan pcmRTPArrival, pcmJitterMaxFrames*2)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		b.readDownlink(arrivals)
	}()
	defer func() { <-readerDone }()
	ticker := time.NewTicker(pcmuFrameDuration)
	defer ticker.Stop()
	defer b.saveJitterStats()
	writeState := &pcmWriteLogState{}
	for {
		select {
		case <-b.closed:
			return
		case arrival, ok := <-arrivals:
			if !ok {
				return
			}
			b.queueDownlink(arrival)
		case <-ticker.C:
			b.playBufferedDownlink(writeState)
		}
	}
}

func (b *PCMBridge) readDownlink(destination chan<- pcmRTPArrival) {
	defer close(destination)
	buf := make([]byte, 2048)
	for {
		if b.conn == nil {
			return
		}
		_ = b.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, addr, err := b.conn.ReadFrom(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		packet, ok := parsePCMURTP(buf[:n])
		if !ok {
			b.lost.Add(1)
			continue
		}
		arrival, accepted := b.acceptArrival(pcmRTPArrival{packet: packet, remote: addr, received: time.Now()})
		if !accepted {
			continue
		}
		select {
		case destination <- arrival:
		case <-b.closed:
			return
		default:
			b.lost.Add(1)
		}
	}
}

func (b *PCMBridge) playDownlink(packet *rtp.Packet, kind pcmPlayoutKind, state *pcmWriteLogState) {
	if kind == pcmPlayoutWaiting || b.pcm == nil {
		return
	}
	samples := make([]int16, pcmuFrameSamples)
	if packet != nil {
		copy(samples, phone.DecodePCMU(packet.Payload))
	} else if kind == pcmPlayoutGap {
		b.lost.Add(1)
	}
	if err := b.pcm.WriteFrame(samples); err != nil {
		b.reportPCMWriteFailure(err, state)
		return
	}
	if kind == pcmPlayoutAudio {
		b.toPCM.Add(1)
	}
}

func (b *PCMBridge) reportPCMWriteFailure(err error, state *pcmWriteLogState) {
	b.overflow.Add(1)
	if state.overflowLogged && time.Since(state.lastOverflow) < 5*time.Second {
		return
	}
	state.overflowLogged, state.lastOverflow = true, time.Now()
	diagnostics := diagnosticsForPCM(b.pcm)
	logger.Warn("VoLTE 上行音频写入模组失败",
		"to_modem", b.toPCM.Load(), "overflow", b.overflow.Load(),
		"alsa_xrun", diagnostics.XRUN, "alsa_recovered", diagnostics.Recovered,
		"alsa_unrecovered", diagnostics.Unrecovered, "err", err)
}

func (b *PCMBridge) setJitterStats(stats pcmJitterDiagnostics) {
	b.jitterMu.Lock()
	b.jitter = stats
	b.jitterMu.Unlock()
}

func (b *PCMBridge) jitterStats() pcmJitterDiagnostics {
	b.jitterMu.Lock()
	defer b.jitterMu.Unlock()
	return b.jitter
}

func (b *PCMBridge) uplink() {
	defer b.workers.Done()
	ticker := time.NewTicker(pcmuFrameDuration)
	defer ticker.Stop()
	for {
		select {
		case <-b.closed:
			return
		case <-ticker.C:
			remote, generation := b.mediaEndpoint()
			if b.conn == nil || remote == nil {
				continue
			}
			samples := make([]int16, pcmuFrameSamples)
			if b.listenOnly || b.pcm == nil {
				b.silent.Add(1)
			} else {
				frame, err := b.pcm.ReadFrame()
				if err != nil {
					b.lost.Add(1)
				} else {
					copy(samples, frame)
					b.fromPCM.Add(1)
				}
			}
			payload := phone.EncodePCMU(samples)
			pkt := encodePCMURTP(b.seq, b.ts, payload)
			b.seq++
			b.ts += pcmuFrameSamples
			if err := b.writeUplink(pkt, generation); err != nil {
				return
			}
		}
	}
}
