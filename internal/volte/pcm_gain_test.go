package volte

import (
	"bytes"
	"net"
	"testing"
	"time"
)

func TestBrowserPCMPlaybackAttenuatesWithoutChangingSilence(t *testing.T) {
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	defer peer.Close()
	pcm := &memPCM{}
	bridge := NewPCMBridge(conn, nil, pcm, false)
	defer bridge.Close()
	// G.711 wire values: +32124, -32124, then silence. Check the actual
	// modem playback frames, rather than a separate gain helper.
	payload := bytes.Repeat([]byte{0xff}, 160)
	payload[0], payload[1] = 0x80, 0x00
	if _, err := peer.WriteTo(encodePCMURTP(1, 0, payload), conn.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for pcm.writtenCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	pcm.mu.Lock()
	defer pcm.mu.Unlock()
	if len(pcm.written) != 1 {
		t.Fatalf("playback frames=%d", len(pcm.written))
	}
	frame := pcm.written[0]
	if len(frame) != 160 || frame[0] != 16062 || frame[1] != -16062 {
		t.Fatalf("incorrect playback gain: %v", frame)
	}
	for i, value := range frame[2:] {
		if value != 0 {
			t.Fatalf("silence sample %d=%d", i+2, value)
		}
	}
}
