package volte

import (
	"context"
	"errors"
	"testing"
)

type idleHandoffStub struct {
	check  func(context.Context) error
	checks int
}

func (h *idleHandoffStub) Validate(ctx context.Context) error { return ctx.Err() }
func (h *idleHandoffStub) CheckIdle(ctx context.Context) error {
	h.checks++
	return h.check(ctx)
}

func TestFailedInitializationCanHandoffWithoutQMI(t *testing.T) {
	h := &handoffHost{FakeModem: newFakeModem(), queryErr: errors.New("no QMI")}
	h.EnsureErr = errors.New("no IMS clients")
	c := NewControllerWithBackup(h, t.TempDir())
	binding := &idleHandoffStub{check: func(context.Context) error { return nil }}
	if err := c.EnableChecked(context.Background(), EnableRequest{DeviceID: "wwan1", IdleHandoff: binding}); err == nil {
		t.Fatal("expected initialization failure")
	}
	if c.Status("wwan1").Phase != PhaseFailed || c.sess["wwan1"] == nil {
		t.Fatal("failure did not retain diagnostic session")
	}
	if err := c.DisableForHandoff(context.Background(), "wwan1"); err != nil {
		t.Fatal(err)
	}
	if binding.checks != 1 || h.hangups != 0 || c.Status("wwan1").Phase != PhaseIdle {
		t.Fatal("AT handoff failed or attempted a hangup")
	}
	if err := c.DisableForHandoff(context.Background(), "wwan1"); err != nil || binding.checks != 1 {
		t.Fatal("repeated handoff touched the modem", err)
	}
	// A new session can bind a new backend after the successful handoff.
	h.EnsureErr = nil
	if err := c.Enable(context.Background(), "wwan1"); err != nil {
		t.Fatal("cannot return to VoLTE", err)
	}
	c.Disable("wwan1")
}

func TestIdleHandoffFailuresRetainSessionForRetry(t *testing.T) {
	for _, stage := range []string{"query", "local_call", "release", "cancel"} {
		t.Run(stage, func(t *testing.T) {
			h := &handoffHost{FakeModem: newFakeModem(), queryErr: errors.New("no QMI")}
			h.EnsureErr = errors.New("no IMS clients")
			c := NewControllerWithBackup(h, t.TempDir())
			binding := &idleHandoffStub{check: func(context.Context) error { return nil }}
			_ = c.EnableChecked(context.Background(), EnableRequest{DeviceID: "wwan1", IdleHandoff: binding})
			s := c.sess["wwan1"]
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch stage {
			case "query":
				binding.check = func(context.Context) error { return errors.New("CLCC failed") }
			case "local_call":
				c.storeCall("wwan1", nativeCall{ID: "call", State: "connected"})
			case "release":
				h.releaseErr = errors.New("release failed")
			case "cancel":
				cancel()
			}
			if err := c.DisableForHandoff(ctx, "wwan1"); err == nil || c.sess["wwan1"] != s {
				t.Fatal("failure discarded session", err)
			}
			if s.voice != nil {
				s.voice.forget("call")
			}
			binding.check = func(context.Context) error { return nil }
			h.releaseErr = nil
			if err := c.DisableForHandoff(context.Background(), "wwan1"); err != nil {
				t.Fatal("retry failed", err)
			}
		})
	}
}

func TestQMIAttemptCannotBecomeATIdleHandoff(t *testing.T) {
	h := &handoffHost{FakeModem: newFakeModem(), queryErr: errors.New("QMI lost")}
	h.VoiceEnsureErr = errors.New("VOICE setup response lost")
	c := NewControllerWithBackup(h, t.TempDir())
	binding := &idleHandoffStub{check: func(context.Context) error { return nil }}
	request := EnableRequest{DeviceID: "wwan1", IdleHandoff: binding}
	if err := c.EnableChecked(context.Background(), request); err == nil {
		t.Fatal("expected VOICE setup failure")
	}
	// A later failure before VOICE setup must not erase the earlier attempt.
	h.EnsureErr = errors.New("IMS unavailable")
	_ = c.EnableChecked(context.Background(), request)
	if err := c.DisableForHandoff(context.Background(), "wwan1"); err == nil || binding.checks != 0 {
		t.Fatal("QMI failure was bypassed using AT", err)
	}
}

func TestQMIQueryCannotBecomeATIdleHandoff(t *testing.T) {
	h := &handoffHost{FakeModem: newFakeModem(), queryErr: errors.New("QMI lost")}
	h.EnsureErr = errors.New("IMS unavailable")
	c := NewControllerWithBackup(h, t.TempDir())
	binding := &idleHandoffStub{check: func(context.Context) error { return nil }}
	_ = c.EnableChecked(context.Background(), EnableRequest{DeviceID: "wwan1", IdleHandoff: binding})
	c.ReconcileCalls(context.Background(), "wwan1")
	if err := c.DisableForHandoff(context.Background(), "wwan1"); err == nil || binding.checks != 0 {
		t.Fatal("QMI query attempt was bypassed", err)
	}
}
