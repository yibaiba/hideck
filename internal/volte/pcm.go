package volte

import (
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"

	"github.com/yibaiba/hideck/internal/phone"
)

const (
	pcmuPayloadType   = 0
	pcmuClockRate     = 8000
	pcmuFrameSamples  = 160
	pcmuFrameDuration = 20 * time.Millisecond
	pcmJitterDepth    = 5
	// Browser-to-modem playback only: halve amplitude (approximately -6 dB).
	pcmPlaybackDivisor = 2
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
	})
	return err
}

func (b *PCMBridge) downlink() {
	defer b.workers.Done()
	buf := make([]byte, 2048)
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
		for i := range pcm {
			pcm[i] /= pcmPlaybackDivisor
		}
		if b.pcm == nil {
			continue
		}
		if err := b.pcm.WriteFrame(pcm); err != nil {
			b.overflow.Add(1)
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

func rtpPCMUPayload(pkt []byte) ([]byte, bool) {
	if len(pkt) < 12 || pkt[0]>>6 != 2 {
		return nil, false
	}
	// Browser RTP retains extensions (audio level, transport-wide sequence,
	// etc.) and may contain padding. Only the decoded payload is audio.
	var packet rtp.Packet
	if packet.Unmarshal(pkt) != nil || packet.PayloadType != pcmuPayloadType || len(packet.Payload) == 0 {
		return nil, false
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
