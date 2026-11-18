package phone

import (
	"testing"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
)

func callStatus(t *testing.T, service *Service, callID string) string {
	t.Helper()
	for _, view := range service.Active("") {
		if view.CallID == callID {
			return view.Status
		}
	}
	return ""
}

// A waiting offer the network withdraws alerts again. It has to leave
// "waiting", otherwise the page keeps offering a media recovery that can
// restore audio but never answer the call.
func TestWithdrawnWaitingOfferCanBeAnsweredAgain(t *testing.T) {
	gateway := newFakeVoiceGateway()
	service := newPhoneTestService(t, gateway, newMemoryCallStore(), time.Second)
	gateway.emitIncoming(voicehost.IncomingCall{DeviceID: "dev-1", CallID: "first", Caller: "10010"})
	gateway.emitEvent(voicehost.CallEvent{Type: "CallAnswered", DeviceID: "dev-1", CallID: "first", Time: time.Now()})
	gateway.emitEvent(voicehost.CallEvent{Type: "CallWaiting", DeviceID: "dev-1", CallID: "second", Caller: "10086", Time: time.Now()})
	if status := callStatus(t, service, "second"); status != StatusWaiting {
		t.Fatalf("second call status=%q", status)
	}
	gateway.emitEvent(voicehost.CallEvent{Type: "CallRinging", DeviceID: "dev-1", CallID: "second", Time: time.Now()})
	if status := callStatus(t, service, "second"); status != StatusRinging {
		t.Fatalf("withdrawn waiting call status=%q", status)
	}
	service.mu.RLock()
	waiting := service.deviceWaiting["dev-1"]
	service.mu.RUnlock()
	if waiting != "" {
		t.Fatalf("device still reserved by a waiting call: %q", waiting)
	}
}
