package volte

import (
	"context"
	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
	"testing"
)

func TestQMIProtocolNumbers(t *testing.T) {
	for _, tt := range []struct {
		raw          uint8
		state, event string
	}{
		{1, "calling", "CallRinging"}, {2, "ringing", "CallRinging"},
		{3, "connected", "CallAnswered"}, {4, "calling", "CallRinging"},
		{5, "ringing", "CallRinging"}, {6, "connected", "CallAnswered"},
		{7, "waiting", "CallWaiting"}, {8, "completed", "CallEnded"},
		{9, "completed", "CallEnded"}, {10, "ringing", "CallRinging"},
	} {
		state, event := mapQMIState(qmi.VoiceCallState(tt.raw))
		if state != tt.state || event != tt.event {
			t.Fatalf("raw %d: %s %s", tt.raw, state, event)
		}
	}
}

func TestQMIRawOutgoingSequence(t *testing.T) {
	for _, early := range []uint8{4, 3} {
		t.Run(map[uint8]string{4: "early-call-control", 3: "early-answer"}[early], func(t *testing.T) {
			ctl, host := enableVoice(t)
			runner := &fakeRunner{}
			audio := NewAudioRuntime(testIdentity("wwan1", "1-2"), runner, fakeCards{"1-2": "fake-card"}, "fake-helper")
			if err := audio.Bind(testIdentity("wwan1", "1-2")); err != nil {
				t.Fatal(err)
			}
			ctl.SetAudioRuntime(audio)
			var events []voicehost.CallEvent
			ctl.SubscribeCallEvents(func(e voicehost.CallEvent) { events = append(events, e) })
			info := func(raw uint8) *qmi.VoiceAllCallInfo {
				return &qmi.VoiceAllCallInfo{Calls: []qmi.VoiceCallInfo{{ID: 1, State: qmi.VoiceCallState(raw), Direction: 1}}}
			}
			host.dialVoice = info(early)
			snap, err := ctl.BeginCall(context.Background(), voicehost.BeginCallRequest{DeviceID: "wwan1", Callee: "10086"})
			if err != nil {
				t.Fatal(err)
			}
			media := ctl.media.get(snap.CallID)
			if media == nil || snap.ClientSDP == "" {
				t.Fatal("missing negotiated media")
			}
			if early == 3 && snap.State != "connected" {
				t.Fatalf("early answer overwritten: %+v", snap)
			}
			for _, raw := range []uint8{4, 1, 5, 3, 3, 9, 9} {
				host.fireVoice(info(raw))
				if early == 4 && raw != 3 && raw != 9 && len(runner.starts) != 0 {
					t.Fatalf("raw %d started audio before conversation", raw)
				}
				if raw == 3 {
					active := ctl.ActiveCall("wwan1")
					if active == nil || active.State != "connected" || active.ClientSDP == "" {
						t.Fatalf("conversation: %+v", active)
					}
					if _, ok := audio.Owner(snap.CallID); !ok {
						t.Fatal("conversation did not attach audio")
					}
				}
			}
			if countType(events, "CallAnswered") != 1 || countType(events, "CallEnded") != 1 {
				t.Fatalf("events: %v", eventTypes(events))
			}
			if len(runner.starts) != 1 || len(runner.stops) != 1 {
				t.Fatalf("audio starts=%d stops=%d", len(runner.starts), len(runner.stops))
			}
			if ctl.ActiveCall("wwan1") != nil || ctl.media.get(snap.CallID) != nil {
				t.Fatal("end retained call/media")
			}
			if _, ok := audio.Owner(snap.CallID); ok {
				t.Fatal("end retained sound card")
			}
			if _, err := media.conn.WriteTo([]byte{0}, media.conn.LocalAddr()); err == nil {
				t.Fatal("end did not close media socket")
			}
			for _, e := range events {
				if e.CallID != snap.CallID {
					t.Fatal("early indication split call identity")
				}
			}
		})
	}
}
