package host

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
	"github.com/yibaiba/hideck/internal/modemvoice"
	"github.com/yibaiba/hideck/internal/modemvoice/media"
)

const twoVoiceCalls = "+CLCC: 3,0,0,0,0,\"10010\",129\n+CLCC: 2,1,5,0,0,\"10086\",129"

type waitingPort struct {
	testPort
	answerErr error
}

func (p *waitingPort) ExecuteATContext(ctx context.Context, command string, timeout time.Duration) (string, error) {
	p.mu.Lock()
	switch command {
	case "AT+CHLD=2":
		p.calls = "+CLCC: 3,0,1,0,0,\"10010\",129\n+CLCC: 2,1,0,0,0,\"10086\",129"
	case "AT+CHLD=13":
		p.calls = "+CLCC: 2,1,0,0,0,\"10086\",129"
	case "AT+CHLD=0":
		p.calls = "+CLCC: 3,0,0,0,0,\"10010\",129"
	default:
		p.mu.Unlock()
		return p.testPort.ExecuteATContext(ctx, command, timeout)
	}
	err := p.answerErr
	p.mu.Unlock()
	if command == "AT+CHLD=2" {
		return "", err
	}
	return "OK", nil
}

func TestWaitingAnswerTransfersAudioAndKeepsHeldCallAddressable(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprint("lost_response=", timeout), func(t *testing.T) { testWaitingAnswer(t, timeout) })
	}
}

func testWaitingAnswer(t *testing.T, lostResponse bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port := &waitingPort{}
	if lostResponse {
		port.answerErr = context.DeadlineExceeded
	}
	session, err := modemvoice.NewSession(ctx, port)
	if err != nil {
		t.Fatal(err)
	}
	route := &testRoute{}
	d := &device{id: "d", ctx: ctx, phase: "ready", session: session, resources: &Resources{Port: port, Check: func(context.Context) error { return nil }, Route: func() (media.AudioRoute, error) { return route, nil }}}
	c := New(Options{Listen: listenTest})
	c.devices["d"] = d
	defer func() {
		if err := c.cleanup(d); err != nil {
			t.Error(err)
		}
	}()
	relayConn, err := listenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer relayConn.Close()
	first, err := c.BeginCall(ctx, voicehost.BeginCallRequest{DeviceID: "d", Callee: "10010", SDP: offer(relayConn)})
	if err != nil {
		t.Fatal(err)
	}
	port.mu.Lock()
	port.calls = twoVoiceCalls
	port.mu.Unlock()
	d.op.Lock()
	update, err := session.Refresh(ctx)
	if err == nil {
		err = c.apply(d, update)
	}
	d.op.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	var waitingID string
	for id := range d.otherCalls {
		waitingID = id
	}
	d.mu.Unlock()
	if waitingID == "" {
		t.Fatal("waiting call not exposed")
	}
	_, answerErr := c.AnswerIncomingCall(ctx, voicehost.AnswerRequest{DeviceID: "d", CallID: waitingID, SDP: offer(relayConn)})
	if lostResponse {
		if !errors.Is(answerErr, context.DeadlineExceeded) {
			t.Fatalf("lost original failure: %v", answerErr)
		}
	} else if answerErr != nil {
		t.Fatal(answerErr)
	}
	if route.closed.Load() {
		t.Fatal("answer closed shared audio route")
	}
	active := c.ActiveCall("d")
	if active == nil || active.CallID != waitingID || active.State != "connected" {
		t.Fatalf("active=%+v", active)
	}
	if err := c.HangupCall(ctx, "d", first.CallID); err != nil {
		t.Fatal(err)
	}
	if active := c.ActiveCall("d"); active == nil || active.CallID != waitingID {
		t.Fatalf("held hangup ended wrong call: %+v", active)
	}
	if err := c.HangupCall(ctx, "d", waitingID); err != nil {
		t.Fatal(err)
	}
}
