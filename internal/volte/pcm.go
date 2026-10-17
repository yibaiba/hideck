package volte

import (
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/yibaiba/hideck/internal/phone"
	"github.com/yibaiba/hideck/pkg/logger"
)

var firstPacketOnce atomic.Bool

const (
	pcmuPayloadType   = 0
	pcmuClockRate     = 8000
	pcmuFrameSamples  = 160
	pcmuFrameDuration = 20 * time.Millisecond
	pcmJitterDepth    = 5
)

type PCMPort interface {
	ReadFrame() ([]int16, error)
	WriteFrame([]int16) error
	Close() error
}

type PCMBridge struct {
	conn       net.PacketConn
	remoteMu   sync.Mutex
	remote     net.Addr
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
}

func NewPCMBridge(conn net.PacketConn, remote net.Addr, pcm PCMPort, listenOnly bool) *PCMBridge {
	b := &PCMBridge{conn: conn, remote: remote, pcm: pcm, listenOnly: listenOnly, closed: make(chan struct{})}
	b.workers.Add(2)
	go b.downlink()
	go b.uplink()
	return b
}

func (b *PCMBridge) setRemote(addr net.Addr) {
	if b == nil || addr == nil {
		return
	}
	b.remoteMu.Lock()
	if b.remote == nil {
		b.remote = addr
	}
	b.remoteMu.Unlock()
}

func (b *PCMBridge) getRemote() net.Addr {
	if b == nil {
		return nil
	}
	b.remoteMu.Lock()
	defer b.remoteMu.Unlock()
	return b.remote
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
		// 挂断即打模组侧统计：toPCM/overflow 才是"音频是否真的进了模组"的
		// 证据，phone 包的 to_modem 只是回环 UDP 写成功计数，不等于送达模组。
		logger.Info("VoLTE 模组音频统计",
			"to_modem", b.toPCM.Load(), "from_modem", b.fromPCM.Load(),
			"silent", b.silent.Load(), "lost", b.lost.Load(), "overflow", b.overflow.Load(),
			"alsa_xrun", alsaPlayXRUN.Load(), "alsa_recovered", alsaPlayRecovered.Load(),
			"alsa_unrecovered", alsaPlayUnrecovered.Load())
	})
	return err
}

func (b *PCMBridge) downlink() {
	defer b.workers.Done()
	buf := make([]byte, 2048)
	var overflowLogged bool
	var lastOverflowLog time.Time
	for {
		select {
		case <-b.closed:
			return
		default:
		}
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
		if addr != nil {
			b.setRemote(addr)
		}
		payload, ok := rtpPCMUPayload(buf[:n])
		if !ok {
			b.lost.Add(1)
			continue
		}
		pcm := phone.DecodePCMU(payload)
		if b.pcm == nil {
			continue
		}
		if err := b.pcm.WriteFrame(pcm); err != nil {
			b.overflow.Add(1)
			// 首次失败就报警，之后每 5s 最多一条，附带模组侧真实计数。
			if !overflowLogged || time.Since(lastOverflowLog) >= 5*time.Second {
				overflowLogged = true
				lastOverflowLog = time.Now()
				logger.Warn("VoLTE 上行音频写入模组失败",
					"to_modem", b.toPCM.Load(), "overflow", b.overflow.Load(),
					"alsa_xrun", alsaPlayXRUN.Load(), "alsa_recovered", alsaPlayRecovered.Load(),
					"alsa_unrecovered", alsaPlayUnrecovered.Load(), "err", err)
			}
			continue
		}
		b.toPCM.Add(1)
	}
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
			remote := b.getRemote()
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
			if _, err := b.conn.WriteTo(pkt, remote); err != nil {
				return
			}
		}
	}
}

// rtpPCMUPayload 取出 PCMU 载荷。必须走完整 RTP 解析：Chrome 的音频包带
// 头扩展（X 位，通常是 ssrc-audio-level / abs-send-time），早期手写版只跳过
// 12 字节固定头，把 8 字节扩展当成 µ-law 音频送进模组——每 20ms 一簇满幅
// 毛刺，对方听到的"电流声"就是它；而录音取的是已解析的 payload，所以录音
// 永远干净，把排查方向一直带偏。padding（P 位）同理必须剥掉。
func rtpPCMUPayload(pkt []byte) ([]byte, bool) {
	var packet rtp.Packet
	if err := packet.Unmarshal(pkt); err != nil {
		return nil, false
	}
	if packet.PayloadType != pcmuPayloadType || len(packet.Payload) == 0 {
		return nil, false
	}
	if firstPacketOnce.CompareAndSwap(false, true) {
		logger.Info("VoLTE 收到首个 RTP 包", "总长度", len(pkt), "载荷长度", len(packet.Payload),
			"扩展", len(packet.Extensions) > 0, "填充", packet.Padding)
	}
	return packet.Payload, true
}

func encodePCMURTP(seq uint16, ts uint32, payload []byte) []byte {
	pkt := make([]byte, 12+len(payload))
	pkt[0] = 0x80
	pkt[1] = pcmuPayloadType
	binary.BigEndian.PutUint16(pkt[2:], seq)
	binary.BigEndian.PutUint32(pkt[4:], ts)
	binary.BigEndian.PutUint32(pkt[8:], 0x48444543)
	copy(pkt[12:], payload)
	return pkt
}
