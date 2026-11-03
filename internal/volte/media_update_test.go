package volte

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

func TestUpdateCallMediaRejectsInvalidState(t *testing.T) {
	for _, scenario := range []string{"device", "call", "ended", "missing", "missing-bridge", "closed", "no-audio", "zero", "negative", "large", "non-number"} {
		t.Run(scenario, func(t *testing.T) {
			remote := rebindUDP(t)
			controller, media, _ := rebindController(t, remote)
			deviceID, callID, sdp := "dev", "volte-call", rebindSDP(remote)
			switch scenario {
			case "device":
				deviceID = "other"
			case "call":
				callID = "other"
			case "ended":
				controller.storeCall(deviceID, nativeCall{ID: callID, State: "completed"})
			case "missing":
				controller.media.take(callID)
			case "missing-bridge":
				controller.media.take(callID)
				controller.media.put(callID, &callMedia{})
			case "closed":
				_ = media.Close()
			case "no-audio":
				sdp = "m=audio-other 1234 RTP/AVP 0"
			case "zero":
				sdp = pcmuOfferSDP(0)
			case "negative":
				sdp = pcmuOfferSDP(-1)
			case "large":
				sdp = pcmuOfferSDP(65536)
			case "non-number":
				sdp = "m=audio invalid RTP/AVP 0"
			}
			if err := controller.UpdateCallMedia(deviceID, callID, sdp); err == nil {
				t.Fatal("invalid media update returned success")
			}
			current, _ := media.bridge.mediaEndpoint()
			if !samePCMEndpoint(current, remote.LocalAddr()) {
				t.Fatal("failed update replaced the valid endpoint")
			}
		})
	}
}

func TestUpdateCallMediaAlwaysUsesLocalRelay(t *testing.T) {
	controller, media, _ := rebindController(t, rebindUDP(t))
	if err := controller.UpdateCallMedia("dev", "volte-call", "c=IN IP4 203.0.113.5\r\nm=audio 12345 RTP/AVP 0\r\n"); err != nil {
		t.Fatal(err)
	}
	endpoint, _ := media.bridge.mediaEndpoint()
	if endpoint.String() != "127.0.0.1:12345" {
		t.Fatalf("used untrusted SDP address: %v", endpoint)
	}
}

func TestUpdateCallMediaRacesWithTeardown(t *testing.T) {
	for _, action := range []string{"hangup", "disable", "close"} {
		t.Run(action, func(t *testing.T) {
			controller, media, _ := rebindController(t, rebindUDP(t))
			next := rebindUDP(t)
			start := make(chan struct{})
			var workers sync.WaitGroup
			workers.Add(2)
			go func() {
				defer workers.Done()
				<-start
				for range 20 {
					_ = controller.ActiveCall("dev")
					_ = controller.UpdateCallMedia("dev", "volte-call", rebindSDP(next))
				}
			}()
			go func() {
				defer workers.Done()
				<-start
				switch action {
				case "hangup":
					_ = controller.HangupCall(context.Background(), "dev", "volte-call")
				case "disable":
					controller.Disable("dev")
				case "close":
					_ = media.Close()
				}
			}()
			close(start)
			workers.Wait()
			if err := controller.UpdateCallMedia("dev", "volte-call", rebindSDP(next)); err == nil {
				t.Fatal("update succeeded after teardown")
			}
		})
	}
}

type blockedPlaybackPCM struct {
	entered chan struct{}
	closed  chan struct{}
}

func (p *blockedPlaybackPCM) ReadFrame() ([]int16, error) { return nil, nil }
func (p *blockedPlaybackPCM) WriteFrame([]int16) error {
	close(p.entered)
	<-p.closed
	return net.ErrClosed
}
func (p *blockedPlaybackPCM) Close() error { close(p.closed); return nil }

func TestPCMRebindWaitsForPlaybackAndCloseUnblocks(t *testing.T) {
	remote, socket := rebindUDP(t), rebindUDP(t)
	pcm := &blockedPlaybackPCM{entered: make(chan struct{}), closed: make(chan struct{})}
	bridge := NewPCMBridge(socket, remote.LocalAddr(), pcm, false)
	defer bridge.Close()
	sendRebindAudio(t, remote, socket.LocalAddr(), 3000)
	select {
	case <-pcm.entered:
	case <-time.After(time.Second):
		t.Fatal("PCM playback did not start")
	}
	updated := make(chan error, 1)
	go func() { updated <- bridge.SetRemote(addr{"new"}) }()
	select {
	case err := <-updated:
		t.Fatalf("rebind returned while old playback was active: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	closed := make(chan struct{})
	go func() { _ = bridge.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close deadlocked behind media lock")
	}
	if err := <-updated; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("pending update on closed bridge = %v", err)
	}
}

func TestUpdateCallMediaAllowsTeardownDuringBlockedPlayback(t *testing.T) {
	for _, action := range []string{"disable", "retire", "handoff"} {
		t.Run(action, func(t *testing.T) {
			controller, bridge := blockedMediaController(t)
			updated := beginBlockedMediaUpdate(t, controller)
			ended := make(chan error, 1)
			go func() {
				switch action {
				case "disable":
					controller.Disable("dev")
					ended <- nil
				case "retire":
					ended <- controller.RetireDevice("dev")
				case "handoff":
					ended <- controller.DisableForHandoff(context.Background(), "dev")
				}
			}()
			select {
			case err := <-ended:
				if err != nil {
					t.Errorf("teardown failed: %v", err)
				}
			case <-time.After(time.Second):
				t.Error("pending media update prevented teardown from closing blocked PCM")
				// Release the blocked I/O only to clean up a failing regression test.
				_ = bridge.Close()
				<-ended
			}
			select {
			case err := <-updated:
				if !errors.Is(err, net.ErrClosed) {
					t.Errorf("update on retired bridge = %v, want closed error", err)
				}
			case <-time.After(time.Second):
				t.Error("pending media update did not exit after teardown")
			}
			if controller.ActiveCall("dev") != nil || controller.media.get("volte-call") != nil {
				t.Error("teardown retained call or media")
			}
		})
	}
}

func blockedMediaController(t *testing.T) (*Controller, *PCMBridge) {
	t.Helper()
	remote, socket := rebindUDP(t), rebindUDP(t)
	pcm := &blockedPlaybackPCM{entered: make(chan struct{}), closed: make(chan struct{})}
	bridge := NewPCMBridge(socket, remote.LocalAddr(), pcm, false)
	t.Cleanup(func() { _ = bridge.Close() })
	controller := NewControllerWithBackup(newFakeModem(), t.TempDir())
	controller.storeCall("dev", nativeCall{ID: "volte-call", QMI: 1, State: "connected"})
	controller.media.put("volte-call", &callMedia{bridge: bridge, conn: socket, pcm: pcm})
	sendRebindAudio(t, remote, socket.LocalAddr(), 3000)
	select {
	case <-pcm.entered:
	case <-time.After(time.Second):
		t.Fatal("PCM playback did not start")
	}
	return controller, bridge
}

func beginBlockedMediaUpdate(t *testing.T, controller *Controller) <-chan error {
	t.Helper()
	// Stop lookup inside the device operation, proving the update precedes teardown.
	controller.media.mu.Lock()
	defer controller.media.mu.Unlock()
	updated := make(chan error, 1)
	go func() { updated <- controller.UpdateCallMedia("dev", "volte-call", pcmuOfferSDP(12345)) }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		controller.mu.Lock()
		operation := controller.locks["dev"]
		controller.mu.Unlock()
		if operation != nil && !operation.TryLock() {
			return updated
		}
		if operation != nil {
			operation.Unlock()
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("media update did not acquire device operation lock")
	return nil
}
