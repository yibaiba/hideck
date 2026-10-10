package phone

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
)

func TestIncomingTerminalClassificationsAndDeduplication(t *testing.T) {
	tests := []struct {
		name       string
		callID     string
		status     string
		answer     bool
		userReject bool
	}{
		{name: "remote cancel is missed", callID: "missed-1", status: StatusMissed},
		{name: "answered call is completed", callID: "completed-1", status: StatusCompleted, answer: true},
		{name: "user reject is rejected", callID: "rejected-1", status: StatusRejected, userReject: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gateway, store := newFakeVoiceGateway(), newMemoryCallStore()
			notifications := make(chan capturedNotification, 2)
			service := newPhoneTestService(t, gateway, store, time.Second)
			service.notifier = captureNotifier{notifications: notifications}
			gateway.emitIncoming(voicehost.IncomingCall{
				DeviceID: "dev-1", CallID: test.callID, Caller: "+15550001",
				OfferSDP: testPlainSDP, ReceivedAt: time.Now(),
			})
			if test.answer {
				gateway.emitEvent(voicehost.CallEvent{
					Type: "CallAnswered", DeviceID: "dev-1", CallID: test.callID, Time: time.Now(),
				})
			}
			if test.userReject {
				addStubMedia(t, service, "media-1", "admin", "lease-1")
				gateway.rejectEmits = true
				if err := service.Reject(ControlRequest{
					Owner: "admin", CallID: test.callID, MediaID: "media-1", Lease: "lease-1",
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				gateway.emitEvent(voicehost.CallEvent{
					Type: "CallCanceled", DeviceID: "dev-1", CallID: test.callID,
					Reason: "remote_cancel", Time: time.Now(),
				})
			}
			waitForRecordStatus(t, store, test.callID, test.status)
			gateway.emitEvent(voicehost.CallEvent{
				Type: "CallCanceled", DeviceID: "dev-1", CallID: test.callID,
				Reason: "duplicate", Time: time.Now(),
			})
			notification := waitForResultNotification(t, notifications)
			if notification.status != test.status {
				t.Fatalf("notification status = %q, want %q", notification.status, test.status)
			}
			deadline := time.After(30 * time.Millisecond)
			for {
				select {
				case duplicate := <-notifications:
					if duplicate.incoming {
						continue
					}
					t.Fatalf("duplicate terminal notification: %+v", duplicate)
				case <-deadline:
					return
				}
			}
		})
	}
}

func TestIncomingTerminalEventBeforeIncomingIsReplayed(t *testing.T) {
	gateway, store := newFakeVoiceGateway(), newMemoryCallStore()
	service := newPhoneTestService(t, gateway, store, time.Second)
	gateway.emitEvent(voicehost.CallEvent{
		Type: "CallCanceled", DeviceID: "dev-1", CallID: "late-1",
		Reason: "remote_cancel", Time: time.Now(),
	})
	gateway.emitIncoming(voicehost.IncomingCall{
		DeviceID: "dev-1", CallID: "late-1", Caller: "+15550001",
		OfferSDP: testPlainSDP, ReceivedAt: time.Now(),
	})
	waitForRecordStatus(t, store, "late-1", StatusMissed)
	if active := service.Active(""); len(active) != 0 {
		t.Fatalf("zombie ringing after early cancel: %+v", active)
	}
}

func TestIncomingRemoteHangupWhileRingingIsMissed(t *testing.T) {
	gateway, store := newFakeVoiceGateway(), newMemoryCallStore()
	service := newPhoneTestService(t, gateway, store, time.Second)
	gateway.emitIncoming(voicehost.IncomingCall{
		DeviceID: "dev-1", CallID: "missed-end-1", Caller: "+15550001",
		OfferSDP: testPlainSDP, ReceivedAt: time.Now(),
	})
	gateway.emitEvent(voicehost.CallEvent{
		Type: "CallEnded", DeviceID: "dev-1", CallID: "missed-end-1",
		Reason: "normal", Time: time.Now(),
	})
	waitForRecordStatus(t, store, "missed-end-1", StatusMissed)
	if active := service.Active(""); len(active) != 0 {
		t.Fatalf("active after remote hangup: %+v", active)
	}
}

func TestIncomingCallNotifiesChannelsWhileRinging(t *testing.T) {
	gateway, store := newFakeVoiceGateway(), newMemoryCallStore()
	notifications := make(chan capturedNotification, 4)
	service := newPhoneTestService(t, gateway, store, time.Second)
	service.notifier = captureNotifier{notifications: notifications}
	gateway.emitIncoming(voicehost.IncomingCall{
		DeviceID: "dev-1", CallID: "ring-1", Caller: "+1555550100", Callee: "10010",
		OfferSDP: testPlainSDP, ReceivedAt: time.Now(),
	})
	select {
	case notification := <-notifications:
		if !notification.incoming || notification.caller != "+1555550100" || notification.callee != "10010" {
			t.Fatalf("incoming notification = %+v", notification)
		}
	case <-time.After(time.Second):
		t.Fatal("incoming call was not notified")
	}
	select {
	case extra := <-notifications:
		t.Fatalf("result notified before hangup: %+v", extra)
	case <-time.After(30 * time.Millisecond):
	}
}

func waitForResultNotification(t *testing.T, notifications <-chan capturedNotification) capturedNotification {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case notification := <-notifications:
			if !notification.incoming {
				return notification
			}
		case <-deadline:
			t.Fatal("terminal notification was not delivered")
		}
	}
}

func TestBusyIncomingCallIsRecordedOnce(t *testing.T) {
	gateway, store := newFakeVoiceGateway(), newMemoryCallStore()
	service := newPhoneTestService(t, gateway, store, time.Second)
	first := voicehost.IncomingCall{DeviceID: "dev-1", CallID: "active-1", ReceivedAt: time.Now()}
	busy := voicehost.IncomingCall{
		DeviceID: "dev-1", CallID: "busy-1", Caller: "+15550002", ReceivedAt: time.Now(),
	}
	gateway.emitIncoming(first)
	gateway.emitIncoming(busy)
	waitForRecordStatus(t, store, busy.CallID, StatusBusy)
	gateway.emitEvent(voicehost.CallEvent{
		Type: "CallBusy", DeviceID: busy.DeviceID, CallID: busy.CallID, Caller: busy.Caller, Time: time.Now(),
	})
	if record := store.record(busy.CallID); record.Status != StatusBusy || record.EndReason != "device_busy" {
		t.Fatalf("busy record = %+v", record)
	}
	select {
	case rejected := <-gateway.rejectCalls:
		if rejected.CallID != busy.CallID || rejected.StatusCode != 486 {
			t.Fatalf("busy rejection = %+v", rejected)
		}
	case <-time.After(time.Second):
		t.Fatal("busy call was not rejected")
	}
	_ = service
}

func TestWaitingIncomingCallWhenConnected(t *testing.T) {
	gateway, store := newFakeVoiceGateway(), newMemoryCallStore()
	service := newPhoneTestService(t, gateway, store, time.Second)
	first := voicehost.IncomingCall{DeviceID: "dev-1", CallID: "active-1", Caller: "+15550001", ReceivedAt: time.Now()}
	gateway.emitIncoming(first)
	gateway.emitEvent(voicehost.CallEvent{
		Type: "CallAnswered", DeviceID: first.DeviceID, CallID: first.CallID, Time: time.Now(),
	})
	waitForRecordStatus(t, store, first.CallID, StatusConnected)
	waiting := voicehost.IncomingCall{
		DeviceID: "dev-1", CallID: "wait-1", Caller: "+15550002", ReceivedAt: time.Now(),
	}
	gateway.emitIncoming(waiting)
	waitForRecordStatus(t, store, waiting.CallID, StatusWaiting)
	select {
	case rejected := <-gateway.rejectCalls:
		t.Fatalf("waiting call was rejected: %+v", rejected)
	default:
	}
	_ = service
}

func TestCallWaitingEventNotifiesAndKeepsSecondCall(t *testing.T) {
	gateway, store := newFakeVoiceGateway(), newMemoryCallStore()
	notifications := make(chan capturedNotification, 4)
	service := newPhoneTestService(t, gateway, store, time.Second)
	service.notifier = captureNotifier{notifications: notifications}
	first := voicehost.IncomingCall{DeviceID: "dev-1", CallID: "active-1", Caller: "+15550001", ReceivedAt: time.Now()}
	gateway.emitIncoming(first)
	gateway.emitEvent(voicehost.CallEvent{
		Type: "CallAnswered", DeviceID: first.DeviceID, CallID: first.CallID, Time: time.Now(),
	})
	waitForRecordStatus(t, store, first.CallID, StatusConnected)
	select {
	case <-notifications:
	case <-time.After(time.Second):
		t.Fatal("first incoming was not notified")
	}
	gateway.emitEvent(voicehost.CallEvent{
		Type: "CallWaiting", DeviceID: "dev-1", CallID: "wait-2",
		Caller: "+15550002", Callee: "10010", Time: time.Now(),
	})
	waitForRecordStatus(t, store, "wait-2", StatusWaiting)
	select {
	case notification := <-notifications:
		if !notification.incoming || notification.caller != "+15550002" {
			t.Fatalf("waiting notification = %+v", notification)
		}
	case <-time.After(time.Second):
		t.Fatal("call waiting was not notified")
	}
}

func TestHoldMapsNotAlignedError(t *testing.T) {
	gateway, store := newFakeVoiceGateway(), newMemoryCallStore()
	gateway.holdErr = voicehost.ErrHoldNotAligned
	service := newPhoneTestService(t, gateway, store, time.Second)
	call := &activeCall{
		view:   CallView{CallID: "call-1", DeviceID: "dev-1", Status: StatusConnected, MediaID: "media-1"},
		record: CallRecord{CallID: "call-1", DeviceID: "dev-1", Status: StatusConnected},
		owner:  "admin", lease: "lease-1", mediaID: "media-1", terminalDone: make(chan struct{}),
	}
	service.mu.Lock()
	service.calls[call.view.CallID] = call
	service.deviceCalls[call.view.DeviceID] = call.view.CallID
	service.mu.Unlock()
	err := service.Hold(context.Background(), "admin", call.view.CallID, "lease-1")
	if !errors.Is(err, ErrHoldUnavailable) {
		t.Fatalf("Hold error = %v", err)
	}
	if service.callView(call.view.CallID, "lease-1").Held {
		t.Fatal("failed hold marked the call held")
	}
}

func TestOutboundEventsEmittedBeforeBeginReturnsAreReplayed(t *testing.T) {
	gateway, store := newFakeVoiceGateway(), newMemoryCallStore()
	gateway.beginEvents = []voicehost.CallEvent{{
		Type: "CallFailed", DeviceID: "dev-1", CallID: "outbound-1", Reason: "busy", Time: time.Now(),
	}}
	service := newPhoneTestService(t, gateway, store, time.Second)
	addStubMedia(t, service, "media-1", "admin", "lease-1")
	call, err := service.StartCall(StartCallRequest{
		Owner: "admin", DeviceID: "dev-1", Callee: "888", MediaID: "media-1", Lease: "lease-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if call.CallID != "outbound-1" {
		t.Fatalf("returned Call-ID = %q", call.CallID)
	}
	waitForRecordStatus(t, store, call.CallID, StatusFailed)
	if call.Status != StatusFailed {
		t.Fatalf("StartCall snapshot status=%q want failed after replayed CallFailed", call.Status)
	}
	if err := service.Hangup(context.Background(), "admin", call.CallID, "lease-1"); err != nil {
		t.Fatalf("hangup of already-ended call: %v", err)
	}
}

func TestHangupPublishesCallEndedWhenGatewayStaysSilent(t *testing.T) {
	gateway, store := newFakeVoiceGateway(), newMemoryCallStore()
	gateway.silentHangup = true
	service := newPhoneTestService(t, gateway, store, time.Second)
	addStubMedia(t, service, "media-1", "admin", "lease-1")
	call, err := service.StartCall(StartCallRequest{
		Owner: "admin", DeviceID: "dev-1", Callee: "888", MediaID: "media-1", Lease: "lease-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, stream, cancel := service.Subscribe(0)
	defer cancel()
	if err := service.Hangup(context.Background(), "admin", call.CallID, "lease-1"); err != nil {
		t.Fatal(err)
	}
	waitForRecordStatus(t, store, call.CallID, StatusFailed)
	if active := service.Active("lease-1"); len(active) != 0 {
		t.Fatalf("active after hangup = %+v", active)
	}
	deadline := time.After(time.Second)
	for {
		select {
		case event := <-stream:
			if event.Type == "call_ended" && event.Call.CallID == call.CallID {
				return
			}
		case <-deadline:
			t.Fatal("call_ended was not published after silent gateway hangup")
		}
	}
}

func TestHangupPublishesCallEndedOnceWhenGatewayAlreadyEmitted(t *testing.T) {
	gateway, store := newFakeVoiceGateway(), newMemoryCallStore()
	service := newPhoneTestService(t, gateway, store, time.Second)
	addStubMedia(t, service, "media-1", "admin", "lease-1")
	call, err := service.StartCall(StartCallRequest{
		Owner: "admin", DeviceID: "dev-1", Callee: "888", MediaID: "media-1", Lease: "lease-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, stream, cancel := service.Subscribe(0)
	defer cancel()
	if err := service.Hangup(context.Background(), "admin", call.CallID, "lease-1"); err != nil {
		t.Fatal(err)
	}
	waitForRecordStatus(t, store, call.CallID, StatusFailed)
	ended := 0
	deadline := time.After(150 * time.Millisecond)
	for {
		select {
		case event := <-stream:
			if event.Type == "call_ended" {
				ended++
			}
		case <-deadline:
			if ended != 1 {
				t.Fatalf("call_ended count = %d, want 1", ended)
			}
			return
		}
	}
}

func TestNewServiceAbandonsIncompleteHistory(t *testing.T) {
	gateway, store := newFakeVoiceGateway(), newMemoryCallStore()
	started := time.Now().Add(-15 * time.Second)
	store.records["ghost-ring"] = CallRecord{
		CallID: "ghost-ring", DeviceID: "wwan0", Direction: "inbound",
		Peer: "+1555550100", Status: StatusRinging, StartedAt: started,
	}
	_ = newPhoneTestService(t, gateway, store, time.Second)
	record := store.record("ghost-ring")
	if record.Status != StatusMissed || record.EndReason != "process_restart" || record.EndedAt == nil {
		t.Fatalf("abandoned record = %+v", record)
	}
}

func TestStartCallCopiesLocalPCMUCodec(t *testing.T) {
	gateway, store := newFakeVoiceGateway(), newMemoryCallStore()
	gateway.beginSnapshot.ClientSDP = "v=0\r\no=hideck 0 0 IN IP4 127.0.0.1\r\ns=HiDeck VoLTE\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 41000 RTP/AVP 0 101\r\na=rtpmap:0 PCMU/8000\r\n"
	service := newPhoneTestService(t, gateway, store, time.Second)
	addStubMedia(t, service, "media-1", "admin", "lease-1")
	call, err := service.StartCall(StartCallRequest{
		Owner: "admin", DeviceID: "dev-1", Callee: "10000", MediaID: "media-1", Lease: "lease-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if call.Codec != "PCMU" {
		t.Fatalf("codec=%q want PCMU from local SDP", call.Codec)
	}
}

func TestControlLeaseProtectsDTMFAndHangup(t *testing.T) {
	gateway, store := newFakeVoiceGateway(), newMemoryCallStore()
	service := newPhoneTestService(t, gateway, store, time.Second)
	call := &activeCall{
		view:   CallView{CallID: "call-1", DeviceID: "dev-1", Status: StatusConnected, MediaID: "media-1"},
		record: CallRecord{CallID: "call-1", DeviceID: "dev-1", Status: StatusConnected},
		owner:  "admin", lease: "lease-1", mediaID: "media-1", terminalDone: make(chan struct{}),
	}
	service.mu.Lock()
	service.calls[call.view.CallID] = call
	service.deviceCalls[call.view.DeviceID] = call.view.CallID
	service.mu.Unlock()
	if err := service.DTMF("admin", call.view.CallID, "wrong", "5"); err == nil {
		t.Fatal("DTMF accepted a foreign lease")
	}
	if err := service.Hangup(context.Background(), "admin", call.view.CallID, "wrong"); err == nil {
		t.Fatal("hangup accepted a foreign lease")
	}
	if err := service.DTMF("admin", call.view.CallID, "lease-1", "5"); err != nil {
		t.Fatal(err)
	}
	if value := <-gateway.dtmfCalls; value != "call-1:5" {
		t.Fatalf("DTMF call = %q", value)
	}
	if active := service.Active("other"); len(active) != 1 || !active[0].ReadOnly {
		t.Fatalf("foreign active view = %+v", active)
	}
	if active := service.Active("lease-1"); len(active) != 1 || active[0].ReadOnly {
		t.Fatalf("owner active view = %+v", active)
	}
	service.publish("call_connected", call)
	backlog, _, cancel := service.Subscribe(0)
	cancel()
	if len(backlog) != 1 || !backlog[0].Call.ReadOnly {
		t.Fatalf("shared event exposed writable view = %+v", backlog)
	}
}

func TestControlLeaseProtectsHoldAndResume(t *testing.T) {
	gateway, store := newFakeVoiceGateway(), newMemoryCallStore()
	service := newPhoneTestService(t, gateway, store, time.Second)
	call := &activeCall{
		view:   CallView{CallID: "call-1", DeviceID: "dev-1", Status: StatusConnected, MediaID: "media-1"},
		record: CallRecord{CallID: "call-1", DeviceID: "dev-1", Status: StatusConnected},
		owner:  "admin", lease: "lease-1", mediaID: "media-1", terminalDone: make(chan struct{}),
	}
	service.mu.Lock()
	service.calls[call.view.CallID] = call
	service.deviceCalls[call.view.DeviceID] = call.view.CallID
	service.mu.Unlock()
	if err := service.Hold(context.Background(), "admin", call.view.CallID, "wrong"); err == nil {
		t.Fatal("hold accepted a foreign lease")
	}
	if err := service.Hold(context.Background(), "admin", call.view.CallID, "lease-1"); err != nil {
		t.Fatal(err)
	}
	if value := <-gateway.holdCalls; value != "call-1:hold" {
		t.Fatalf("hold call = %q", value)
	}
	if !service.callView(call.view.CallID, "lease-1").Held {
		t.Fatal("hold did not mark the call held")
	}
	if err := service.Resume(context.Background(), "admin", call.view.CallID, "lease-1"); err != nil {
		t.Fatal(err)
	}
	if value := <-gateway.holdCalls; value != "call-1:resume" {
		t.Fatalf("resume call = %q", value)
	}
}

func TestAnswerRejectsIncomingCallWithUnavailableCodec(t *testing.T) {
	gateway, store := newFakeVoiceGateway(), newMemoryCallStore()
	service := newPhoneTestService(t, gateway, store, time.Second)
	gateway.emitIncoming(voicehost.IncomingCall{
		DeviceID: "dev-1", CallID: "unsupported-1", Caller: "+15550003",
		OfferSDP:   "v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 41000 RTP/AVP 104\r\na=rtpmap:104 AMR-WB/16000\r\na=fmtp:104 octet-align=1\r\n",
		ReceivedAt: time.Now(),
	})
	addStubMedia(t, service, "media-1", "admin", "lease-1")
	_, err := service.Answer(context.Background(), ControlRequest{
		Owner: "admin", CallID: "unsupported-1", MediaID: "media-1", Lease: "lease-1",
	})
	if err == nil {
		t.Fatal("unsupported incoming codec was accepted")
	}
	select {
	case rejected := <-gateway.rejectCalls:
		if rejected.CallID != "unsupported-1" || rejected.StatusCode != 488 {
			t.Fatalf("codec rejection = %+v", rejected)
		}
	case <-time.After(time.Second):
		t.Fatal("unsupported incoming codec was not rejected")
	}
}

const testPlainSDP = "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=phone\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 40000 RTP/AVP 0\r\n"

func TestEarlyOutboundAnswerAttachesMediaBeforeDeduplicating(t *testing.T) {
	gateway, store := newFakeVoiceGateway(), newMemoryCallStore()
	gateway.beginSnapshot.State = "connected"
	gateway.beginSnapshot.ClientSDP = testPlainSDP
	gateway.activeSnapshots["dev-1"] = gateway.beginSnapshot
	gateway.beginEvents = []voicehost.CallEvent{{Type: "CallAnswered", DeviceID: "dev-1", CallID: "outbound-1", Time: time.Now()}}
	service := newPhoneTestService(t, gateway, store, time.Second)
	addStubMedia(t, service, "media-1", "admin", "lease-1")
	call, err := service.StartCall(StartCallRequest{Owner: "admin", DeviceID: "dev-1", Callee: "888", MediaID: "media-1", Lease: "lease-1"})
	if err != nil {
		t.Fatal(err)
	}
	if call.Status != StatusConnected || call.AnsweredAt == nil {
		t.Fatalf("early answer lost: %+v", call)
	}
	media := service.media.Get("media-1")
	media.mu.RLock()
	attached := media.attached
	media.mu.RUnlock()
	if !attached {
		t.Fatal("early answer marked answered without media attachment")
	}
	gateway.emitEvent(gateway.beginEvents[0])
	if active := service.Active("lease-1"); len(active) != 1 || active[0].Status != StatusConnected {
		t.Fatalf("duplicate answer: %+v", active)
	}
}
