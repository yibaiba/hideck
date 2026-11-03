package phone

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type mediaUpdateGateway struct {
	*fakeVoiceGateway
	update func(string) error
}

func (g *mediaUpdateGateway) UpdateCallMedia(_, _, sdp string) error { return g.update(sdp) }

func newRefreshTestService(t *testing.T, update func(string) error) *Service {
	t.Helper()
	gateway := newFakeVoiceGateway()
	service := newPhoneTestService(t, gateway, newMemoryCallStore(), time.Second)
	service.gateway = &mediaUpdateGateway{fakeVoiceGateway: gateway, update: update}
	addStubMedia(t, service, "old", "admin", "old-lease")
	addStubMedia(t, service, "first", "admin", "first-lease")
	addStubMedia(t, service, "second", "admin", "second-lease")
	service.calls["call"] = &activeCall{
		view:  CallView{CallID: "call", DeviceID: "dev-1", MediaID: "old"},
		owner: "admin", lease: "old-lease", mediaID: "old", incomingSDP: testPlainSDP,
		terminalDone: make(chan struct{}), finalizedDone: make(chan struct{}),
	}
	service.mediaCalls["old"] = "call"
	return service
}

func TestRefreshFailurePreservesOldMedia(t *testing.T) {
	failure := errors.New("native bridge is closed")
	service := newRefreshTestService(t, func(string) error { return failure })
	old := service.media.Get("old")
	_, _, err := service.RefreshMedia(RefreshRequest{Owner: "admin", CallID: "call", MediaID: "first", Takeover: true})
	if !errors.Is(err, failure) {
		t.Fatalf("update failure hidden: %v", err)
	}
	if service.calls["call"].mediaID != "old" || service.calls["call"].lease != "old-lease" || sessionClosed(old.closed) {
		t.Fatal("failed rebind invalidated old session or control lease")
	}
	if service.mediaCalls["old"] != "call" || len(service.mediaReservations) != 0 {
		t.Fatal("failed rebind corrupted media ownership")
	}
}

func TestRefreshAttachFailureDoesNotUpdateBackend(t *testing.T) {
	var updates atomic.Int32
	service := newRefreshTestService(t, func(string) error { updates.Add(1); return nil })
	service.calls["call"].incomingSDP = "invalid SDP"
	if _, _, err := service.RefreshMedia(RefreshRequest{Owner: "admin", CallID: "call", MediaID: "first", Takeover: true}); err == nil {
		t.Fatal("invalid negotiated media accepted")
	}
	if updates.Load() != 0 || sessionClosed(service.media.Get("old").closed) {
		t.Fatal("backend moved before new session was ready")
	}
}

func TestRefreshSerializesTakeoversAndLeaseChecks(t *testing.T) {
	for _, takeover := range []bool{true, false} {
		t.Run(map[bool]string{true: "takeovers", false: "stale-lease"}[takeover], func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			var updates atomic.Int32
			service := newRefreshTestService(t, func(string) error {
				if updates.Add(1) == 1 {
					close(entered)
					<-release
				}
				return nil
			})
			old, first := service.media.Get("old"), service.media.Get("first")
			result := make(chan error, 1)
			go func() {
				_, _, err := service.RefreshMedia(RefreshRequest{Owner: "admin", CallID: "call", MediaID: "first", Takeover: true})
				result <- err
			}()
			<-entered
			second := make(chan error, 1)
			go func() {
				_, _, err := service.RefreshMedia(RefreshRequest{Owner: "admin", CallID: "call", MediaID: "second", Lease: "old-lease", Takeover: takeover})
				second <- err
			}()
			select {
			case err := <-second:
				close(release)
				t.Fatalf("second refresh escaped serialization: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			if updates.Load() != 1 || sessionClosed(old.closed) {
				t.Fatal("concurrent refresh updated backend or prematurely closed old media")
			}
			close(release)
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			err := <-second
			if takeover {
				if err != nil || updates.Load() != 2 || service.calls["call"].mediaID != "second" || !sessionClosed(first.closed) {
					t.Fatalf("takeovers did not commit in order: %v", err)
				}
			} else if err == nil || updates.Load() != 1 || service.calls["call"].mediaID != "first" || sessionClosed(first.closed) {
				t.Fatalf("stale lease displaced the current session: %v", err)
			}
			if !sessionClosed(old.closed) {
				t.Fatal("successful refresh retained obsolete media")
			}
		})
	}
}
