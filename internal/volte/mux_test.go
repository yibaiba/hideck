package volte

import (
	"context"
	"testing"

	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
)

type stubBackend struct {
	name string
	last string
}

func (s *stubBackend) SubscribeIncomingCalls(func(voicehost.IncomingCall)) func() {
	return func() {}
}
func (s *stubBackend) SubscribeCallEvents(func(voicehost.CallEvent)) func() { return func() {} }
func (s *stubBackend) BeginCall(_ context.Context, req voicehost.BeginCallRequest) (voicehost.CallSnapshot, error) {
	s.last = req.DeviceID
	return voicehost.CallSnapshot{CallID: s.name + "-" + req.DeviceID, DeviceID: req.DeviceID}, nil
}
func (s *stubBackend) ActiveCall(string) *voicehost.CallSnapshot { return nil }
func (s *stubBackend) AnswerIncomingCall(context.Context, voicehost.AnswerRequest) (voicehost.AnswerResult, error) {
	return voicehost.AnswerResult{}, nil
}
func (s *stubBackend) RejectIncomingCall(voicehost.RejectRequest) error { return nil }
func (s *stubBackend) HangupCall(context.Context, string, string) error { return nil }
func (s *stubBackend) SendCallDTMF(string, string, string) error        { return nil }
func (s *stubBackend) HoldCall(context.Context, string, string) error   { return nil }
func (s *stubBackend) ResumeCall(context.Context, string, string) error { return nil }
func (s *stubBackend) SwitchCall(string, string) error                  { return nil }
func (s *stubBackend) StartCallCapture(string, string, string) error    { return nil }
func (s *stubBackend) DeviceStatus(string) map[string]interface{} {
	return map[string]interface{}{"backend": s.name}
}

func TestMuxKeepsCellularOnSoftwareIMS(t *testing.T) {
	ims := &stubBackend{name: "ims"}
	native := &stubBackend{name: "native"}
	mux := &Mux{IMS: ims, Native: native, IsNative: func(id string) bool { return id == "volte-dev" }}
	got, err := mux.BeginCall(context.Background(), voicehost.BeginCallRequest{DeviceID: "cellular-dev"})
	if err != nil || got.CallID != "ims-cellular-dev" {
		t.Fatalf("cellular must stay on software IMS: %+v err=%v", got, err)
	}
}

func TestMuxRoutesByMode(t *testing.T) {
	ims := &stubBackend{name: "ims"}
	native := &stubBackend{name: "native"}
	mux := &Mux{IMS: ims, Native: native, IsNative: func(id string) bool { return id == "wwan1" }}

	got, err := mux.BeginCall(context.Background(), voicehost.BeginCallRequest{DeviceID: "wwan0"})
	if err != nil || got.CallID != "ims-wwan0" {
		t.Fatalf("wifi path: %+v err=%v", got, err)
	}
	got, err = mux.BeginCall(context.Background(), voicehost.BeginCallRequest{DeviceID: "wwan1"})
	if err != nil || got.CallID != "native-wwan1" {
		t.Fatalf("volte path: %+v err=%v", got, err)
	}
}

func TestMuxRetainsModemCallOwnerAfterModeSwitch(t *testing.T) {
	ims, native, modem := &stubBackend{name: "ims"}, &stubBackend{name: "native"}, &stubBackend{name: "modem"}
	selected := true
	mux := &Mux{IMS: ims, Native: native, Modem: modem, IsModem: func(string) bool { return selected }}
	got, err := mux.BeginCall(context.Background(), voicehost.BeginCallRequest{DeviceID: "d"})
	if err != nil || got.CallID != "modem-d" {
		t.Fatalf("%+v %v", got, err)
	}
	selected = false
	if mux.pickCall("d", "modemvoice-owned") != modem {
		t.Fatal("call followed new mode")
	}
	if mux.pick("d") != ims {
		t.Fatal("new calls did not follow selected mode")
	}
}

func TestMapQMIState(t *testing.T) {
	state, event := mapQMIState(qmiCallConversation)
	if state != "connected" || event != "CallAnswered" {
		t.Fatalf("conversation -> %s %s", state, event)
	}
	state, event = mapQMIState(qmiCallEnd)
	if state != "completed" || event != "CallEnded" {
		t.Fatalf("end -> %s %s", state, event)
	}
	state, event = mapQMIState(qmiCallOriginating)
	if state != "calling" || event != "CallRinging" {
		t.Fatalf("originating -> %s %s", state, event)
	}
	state, event = mapQMIState(qmiCallDisconnecting)
	if state != "completed" || event != "CallEnded" {
		t.Fatalf("disconnecting -> %s %s", state, event)
	}
	state, event = mapQMIState(qmiCallIdle)
	if state != "completed" || event != "CallEnded" {
		t.Fatalf("idle -> %s %s", state, event)
	}
	state, event = mapQMIState(qmiCallSetup)
	if state != "ringing" || event != "CallRinging" {
		t.Fatalf("setup -> %s %s", state, event)
	}
}

func TestMuxMediaUpdatePropagatesNativeFailure(t *testing.T) {
	controller := NewControllerWithBackup(newFakeModem(), t.TempDir())
	mux := &Mux{IMS: &stubBackend{}, Native: controller}
	if err := mux.UpdateCallMedia("dev", "volte-missing", pcmuOfferSDP(40000)); err == nil {
		t.Fatal("native media update was silently ignored")
	}
	if err := mux.UpdateCallMedia("dev", "ims-call", pcmuOfferSDP(40000)); err != nil {
		t.Fatalf("legacy backend recovery changed: %v", err)
	}
}
