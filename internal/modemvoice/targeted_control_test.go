package modemvoice

import (
	"context"
	"testing"
)

func TestTargetedControlKeepsOtherCall(t *testing.T) {
	const snapshot = "+CLCC: 1,0,0,0,0,\"10010\",129\n+CLCC: 2,1,5,0,0,\"10086\",129"
	for _, tt := range []struct {
		name   string
		index  int
		answer bool
		want   string
	}{
		{"hangup active", 0, false, "AT+CHLD=11"},
		{"reject waiting", 1, false, "AT+CHLD=0"},
		{"answer waiting", 1, true, "AT+CHLD=2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := newSessionPort()
			p.response = snapshot
			s := newTestSession(t, p)
			if _, err := s.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			id := s.Calls()[tt.index].ID
			var err error
			if tt.answer {
				_, err = s.Answer(context.Background(), id)
			} else {
				_, err = s.Hangup(context.Background(), id)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := p.commands[len(p.commands)-1]; got != tt.want {
				t.Fatalf("command=%q want=%q", got, tt.want)
			}
		})
	}
}

func TestTargetedControlRejectsReusedIndex(t *testing.T) {
	p := newSessionPort()
	p.response = "+CLCC: 1,0,0,0,0,\"10010\",129\n+CLCC: 2,1,5,0,0,\"10086\",129"
	s := newTestSession(t, p)
	if _, err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	id := s.Calls()[0].ID
	p.response = "+CLCC: 1,0,2,0,0,\"10000\",129\n+CLCC: 2,1,5,0,0,\"10086\",129"
	update, err := s.Hangup(context.Background(), id)
	if err == nil || update.Attempted {
		t.Fatalf("controlled reused index: %+v %v", update, err)
	}
}
