package host

import (
	"context"
	"errors"
	"testing"

	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
	"github.com/yibaiba/hideck/internal/modemvoice/media"
)

func manualController(t *testing.T, d *device) *Controller {
	t.Helper()
	c := New(Options{Listen: listenTest})
	c.devices[d.id] = d
	t.Cleanup(func() {
		if err := c.cleanup(d); err != nil {
			t.Error(err)
		}
	})
	return c
}

// A call the modem already shows as active has an endpoint but no bridge when
// nothing went through Answer: accepting a waiting call with CHLD makes the
// second call live. Recovery must open the bridge, not reject the browser.
func TestUpdateCallMediaOpensBridgeTheModemSkipped(t *testing.T) {
	route := &testRoute{}
	c := manualController(t, &device{id: "d", ctx: context.Background(), phase: "ready",
		resources: &Resources{Route: func() (media.AudioRoute, error) { return route, nil }}})
	hostConn, err := listenTest()
	if err != nil {
		t.Fatal(err)
	}
	relayConn, err := listenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer relayConn.Close()
	current := &call{trackedID: "1", conn: hostConn,
		snapshot: voicehost.CallSnapshot{CallID: "modemvoice-live", DeviceID: "d", State: "connected", ClientSDP: offer(hostConn)}}
	c.get("d").mu.Lock()
	c.get("d").call = current
	c.get("d").mu.Unlock()
	if err := c.UpdateCallMedia("d", "modemvoice-live", offer(relayConn)); err != nil {
		t.Fatal(err)
	}
	c.get("d").mu.Lock()
	bridge := current.bridge
	c.get("d").mu.Unlock()
	if bridge == nil {
		t.Fatal("recovery left the call without audio")
	}
	if route.closed.Load() {
		t.Fatal("audio route was replaced instead of reused")
	}
}

func TestUpdateCallMediaKeepsWaitingCallOffTheAudioRoute(t *testing.T) {
	c := manualController(t, &device{id: "d", ctx: context.Background(), phase: "ready",
		resources: &Resources{Route: func() (media.AudioRoute, error) { return &testRoute{}, nil }}})
	foregroundConn, err := listenTest()
	if err != nil {
		t.Fatal(err)
	}
	waitingConn, err := listenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer waitingConn.Close()
	relayConn, err := listenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer relayConn.Close()
	d := c.get("d")
	foreground := &call{trackedID: "1", conn: foregroundConn,
		snapshot: voicehost.CallSnapshot{CallID: "modemvoice-active", DeviceID: "d", State: "connected"}}
	waiting := &call{trackedID: "2", conn: waitingConn,
		snapshot: voicehost.CallSnapshot{CallID: "modemvoice-waiting", DeviceID: "d", State: "waiting"}}
	d.mu.Lock()
	d.call = foreground
	d.otherCalls = map[string]*call{"modemvoice-waiting": waiting}
	d.mu.Unlock()
	if err := c.UpdateCallMedia("d", "modemvoice-waiting", offer(relayConn)); !errors.Is(err, errAudioInUse) {
		t.Fatalf("waiting call took the active route: %v", err)
	}
	d.mu.Lock()
	bridge := waiting.bridge
	d.mu.Unlock()
	if bridge != nil {
		t.Fatal("waiting call opened a second bridge")
	}
}

// A waiting call the modem activated on its own has no endpoint at all, so the
// phone service asks for one instead of guessing.
func TestAttachCallMediaBuildsEndpointForActivatedWaitingCall(t *testing.T) {
	c := manualController(t, &device{id: "d", ctx: context.Background(), phase: "ready",
		resources: &Resources{Route: func() (media.AudioRoute, error) { return &testRoute{}, nil }}})
	relayConn, err := listenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer relayConn.Close()
	d := c.get("d")
	waiting := &call{trackedID: "2",
		snapshot: voicehost.CallSnapshot{CallID: "modemvoice-waiting", DeviceID: "d", State: "connected"}}
	d.mu.Lock()
	d.otherCalls = map[string]*call{"modemvoice-waiting": waiting}
	d.mu.Unlock()
	offerSDP, err := c.AttachCallMedia("d", "modemvoice-waiting", offer(relayConn))
	if err != nil {
		t.Fatal(err)
	}
	if offerSDP == "" {
		t.Fatal("no endpoint offered to the browser")
	}
	if snapshot := c.ActiveCall("d"); snapshot == nil || snapshot.CallID != waiting.snapshot.CallID {
		t.Fatal("recovered call does not own foreground cleanup")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if waiting.bridge == nil || waiting.conn == nil || waiting.snapshot.ClientSDP != offerSDP {
		t.Fatal("audio endpoint was not retained for the call")
	}
}

func TestAttachCallMediaKeepsHeldAndWaitingCallsOffRoute(t *testing.T) {
	for _, state := range []string{"held", "waiting"} {
		t.Run(state, func(t *testing.T) {
			c := manualController(t, &device{id: "d", ctx: context.Background()})
			d := c.get("d")
			current := &call{snapshot: voicehost.CallSnapshot{CallID: "modemvoice-2", State: "waiting"}}
			if state == "held" {
				current.snapshot.State, current.snapshot.Held = "connected", true
			}
			d.call = current
			if _, err := c.AttachCallMedia("d", "modemvoice-2", ""); !errors.Is(err, errAudioInUse) {
				t.Fatalf("inactive call acquired audio: %v", err)
			}
		})
	}
}
