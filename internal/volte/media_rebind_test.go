package volte

import (
	"net"
	"testing"
	"time"

	"github.com/yibaiba/hideck/internal/phone"
)

type rebindPCM struct {
	written chan []int16
	closed  chan struct{}
}

func (p *rebindPCM) ReadFrame() ([]int16, error) { return rebindTone(2400), nil }
func (p *rebindPCM) WriteFrame(frame []int16) error {
	select {
	case p.written <- append([]int16(nil), frame...):
		return nil
	case <-p.closed:
		return net.ErrClosed
	}
}
func (p *rebindPCM) Close() error { close(p.closed); return nil }

func rebindTone(sample int16) []int16 {
	frame := make([]int16, pcmuFrameSamples)
	for i := range frame {
		frame[i] = sample
	}
	return frame
}

func rebindUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func rebindSDP(conn *net.UDPConn) string {
	return pcmuOfferSDP(conn.LocalAddr().(*net.UDPAddr).Port)
}

func rebindController(t *testing.T, remote *net.UDPConn) (*Controller, *callMedia, *rebindPCM) {
	t.Helper()
	pcm := &rebindPCM{written: make(chan []int16, 128), closed: make(chan struct{})}
	media, err := startCallMedia(rebindSDP(remote), pcm, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = media.Close() })
	controller := NewControllerWithBackup(newFakeModem(), t.TempDir())
	controller.storeCall("dev", nativeCall{ID: "volte-call", QMI: 1, State: "connected", ClientSDP: media.sdp})
	controller.media.put("volte-call", media)
	return controller, media, pcm
}

func readRebindAudio(t *testing.T, conn *net.UDPConn) (uint16, uint32) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	raw := make([]byte, 2048)
	n, _, err := conn.ReadFromUDP(raw)
	if err != nil {
		t.Fatal(err)
	}
	packet, ok := parsePCMURTP(raw[:n])
	if !ok || phone.DecodePCMU(packet.Payload)[0] <= 0 {
		t.Fatal("new relay did not receive nonzero PCM audio")
	}
	return packet.SequenceNumber, packet.Timestamp
}

func sendRebindAudio(t *testing.T, conn *net.UDPConn, target net.Addr, sample int16) {
	t.Helper()
	// Every takeover deliberately reuses SSRC and resets sequence/timestamp.
	for sequence := uint16(1); sequence <= pcmJitterInitialFrames; sequence++ {
		packet := encodePCMURTP(sequence, uint32(sequence)*pcmuFrameSamples, phone.EncodePCMU(rebindTone(sample)))
		if _, err := conn.WriteTo(packet, target); err != nil {
			t.Fatal(err)
		}
	}
}

func drainRebindPCM(pcm *rebindPCM) {
	for {
		select {
		case <-pcm.written:
		default:
			return
		}
	}
}

func TestNativeMuxRebindsBidirectionalAudio(t *testing.T) {
	old := rebindUDP(t)
	controller, media, pcm := rebindController(t, old)
	// Existing calls must still route to native even after a device mode change.
	mux := &Mux{IMS: &stubBackend{}, Native: controller, IsNative: func(string) bool { return false }}
	previousSeq, previousTS := readRebindAudio(t, old)
	for range 3 {
		next := rebindUDP(t)
		if err := mux.UpdateCallMedia("dev", "volte-call", rebindSDP(next)); err != nil {
			t.Fatal(err)
		}
		sequence, timestamp := readRebindAudio(t, next)
		if sequence <= previousSeq || timestamp-previousTS != uint32(sequence-previousSeq)*pcmuFrameSamples {
			t.Fatalf("RTP timeline reset: %d/%d -> %d/%d", previousSeq, previousTS, sequence, timestamp)
		}
		assertOldRelayQuiet(t, old)
		drainRebindPCM(pcm)
		sendRebindAudio(t, old, media.conn.LocalAddr(), 6000)
		sendRebindAudio(t, next, media.conn.LocalAddr(), -3000)
		assertNewPCMPlayback(t, pcm)
		previousSeq, previousTS = sequence, timestamp
		old = next
	}
}

func assertOldRelayQuiet(t *testing.T, conn *net.UDPConn) {
	t.Helper()
	// Drain datagrams sent before SetRemote returned, then demand a quiet interval.
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
	raw := make([]byte, 2048)
	for {
		if _, _, err := conn.ReadFromUDP(raw); err != nil {
			if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
				t.Fatal(err)
			}
			break
		}
	}
	_ = conn.SetReadDeadline(time.Now().Add(80 * time.Millisecond))
	if _, _, err := conn.ReadFromUDP(raw); err == nil {
		t.Fatal("old relay still receives new audio after takeover")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatal(err)
	}
}

func assertNewPCMPlayback(t *testing.T, pcm *rebindPCM) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case frame := <-pcm.written:
			if frame[0] > 0 {
				t.Fatal("old prompt reached PCM after takeover")
			}
			if frame[0] < 0 {
				return
			}
		case <-timer.C:
			t.Fatal("new relay audio was not decoded into PCM")
		}
	}
}

func TestPCMRebindDiscardsQueuedGenerationAndResetsStream(t *testing.T) {
	old, next := addr{"old"}, addr{"next"}
	bridge := &PCMBridge{remote: old, buffer: newAdaptivePCMBuffer(), closed: make(chan struct{})}
	packet, _ := parsePCMURTP(encodePCMURTP(100, 16000, phone.EncodePCMU(rebindTone(3000))))
	arrival, ok := bridge.acceptArrival(pcmRTPArrival{packet: packet, remote: old, received: time.Now()})
	if !ok {
		t.Fatal("initial source rejected")
	}
	bridge.queueDownlink(arrival)
	if err := bridge.SetRemote(old); err != nil || len(bridge.buffer.packets) != 1 {
		t.Fatal("same-endpoint update reset the jitter buffer")
	}
	if err := bridge.SetRemote(next); err != nil {
		t.Fatal(err)
	}
	if bridge.buffer.started || len(bridge.buffer.packets) != 0 || bridge.buffer.diagnostics.Received != 1 {
		t.Fatal("stream state was retained or cumulative diagnostics were lost")
	}
	// Returning to the same address must not make a queued previous generation valid.
	if err := bridge.SetRemote(old); err != nil {
		t.Fatal(err)
	}
	bridge.queueDownlink(arrival)
	if len(bridge.buffer.packets) != 0 {
		t.Fatal("queued previous generation contaminated new stream")
	}
	packet.SequenceNumber = 1
	fresh, _ := bridge.acceptArrival(pcmRTPArrival{packet: packet, remote: old, received: time.Now()})
	bridge.queueDownlink(fresh)
	if bridge.buffer.expected != 1 || bridge.buffer.diagnostics.Received != 2 {
		t.Fatal("new stream with reused SSRC and reset sequence rejected")
	}
}

func TestPCMEndpointDiscoveryOnlyAcceptsFirstSource(t *testing.T) {
	bridge := &PCMBridge{closed: make(chan struct{})}
	if _, ok := bridge.acceptArrival(pcmRTPArrival{remote: addr{"first"}}); !ok {
		t.Fatal("initial endpoint not discovered")
	}
	if _, ok := bridge.acceptArrival(pcmRTPArrival{remote: addr{"second"}}); ok {
		t.Fatal("unsignalled source replaced the endpoint")
	}
}
