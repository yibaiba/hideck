package volte

import (
	"testing"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
)

// TestQMICallStateMatchesSpec 把枚举值钉在 libqmi/3GPP 的规范顺序上：
// IDLE, ORIG, INCOMING, CONV, CC_IN_PROG, ALERTING, HOLD, WAITING,
// DISCONNECTING, END, SETUP。
// 这张表曾经整体错位一位：0x04（CC_IN_PROGRESS，早媒体/回铃）被当成
// CONVERSATION，于是拨号后十几毫秒就发 CallAnswered，录音从回铃音开始，
// 铃声被录进通话记录里。
func TestQMICallStateMatchesSpec(t *testing.T) {
	cases := []struct {
		state qmi.VoiceCallState
		want  byte
		name  string
	}{
		{qmiCallIdle, 0x00, "idle"},
		{qmiCallOriginating, 0x01, "originating"},
		{qmiCallIncoming, 0x02, "incoming"},
		{qmiCallConversation, 0x03, "conversation"},
		{qmiCallCCInProgress, 0x04, "cc_in_progress"},
		{qmiCallAlerting, 0x05, "alerting"},
		{qmiCallHolding, 0x06, "hold"},
		{qmiCallWaiting, 0x07, "waiting"},
		{qmiCallDisconnecting, 0x08, "disconnecting"},
		{qmiCallEnd, 0x09, "end"},
		{qmiCallSetup, 0x0a, "setup"},
	}
	for _, item := range cases {
		if byte(item.state) != item.want {
			t.Fatalf("%s = %#x, want %#x", item.name, byte(item.state), item.want)
		}
	}
}

// TestMapQMIStateEarlyMediaIsNotAnswered 早媒体阶段必须仍是振铃：
// 一旦在这里发 CallAnswered，通话录音会把回铃音一起录进去。
func TestMapQMIStateEarlyMediaIsNotAnswered(t *testing.T) {
	state, event := mapQMIState(qmiCallCCInProgress)
	if state != "ringing" || event != "CallRinging" {
		t.Fatalf("cc_in_progress mapped to %q/%q, want ringing/CallRinging", state, event)
	}
	for _, ringing := range []qmi.VoiceCallState{qmiCallIncoming, qmiCallAlerting, qmiCallSetup} {
		if state, event := mapQMIState(ringing); state != "ringing" || event != "CallRinging" {
			t.Fatalf("%#x mapped to %q/%q, want ringing/CallRinging", byte(ringing), state, event)
		}
	}
	if state, event := mapQMIState(qmiCallConversation); state != "connected" || event != "CallAnswered" {
		t.Fatalf("conversation mapped to %q/%q, want connected/CallAnswered", state, event)
	}
	if state, event := mapQMIState(qmiCallEnd); state != "completed" || event != "CallEnded" {
		t.Fatalf("end mapped to %q/%q, want completed/CallEnded", state, event)
	}
}
