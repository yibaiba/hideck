package device

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	qmimanager "github.com/iniwex5/quectel-qmi-go/pkg/manager"
	"github.com/yibaiba/hideck/internal/backend"
	"github.com/yibaiba/hideck/internal/cardpolicy"
	"github.com/yibaiba/hideck/internal/volte"
)

type pendingNativeReadiness struct {
	backend.DeviceBackend
	entered, release chan struct{}
}

func (*pendingNativeReadiness) Mode() string { return backend.BackendQMI }
func (b *pendingNativeReadiness) GetUIMReadiness(ctx context.Context) (qmimanager.UIMReadiness, error) {
	close(b.entered)
	select {
	case <-b.release:
	case <-ctx.Done():
		return qmimanager.UIMReadiness{}, ctx.Err()
	}
	return qmimanager.UIMReadiness{Reason: qmimanager.UIMReadinessReady}, nil
}

type nativeStartEffectHost struct {
	volte.Host
	starts  atomic.Int32
	started chan struct{}
}

func (h *nativeStartEffectHost) StopSoftwareIMS(string) error {
	if h.starts.Add(1) == 1 && h.started != nil {
		close(h.started)
	}
	return errors.New("test stops provisioning before hardware access")
}
func (*nativeStartEffectHost) AudioDevice(string) string      { return "" }
func (*nativeStartEffectHost) ReleaseIMSClients(string) error { return nil }

func TestNativeStartCanceledDuringReadinessCannotOutliveHandoff(t *testing.T) {
	p, port := testModemVoicePort(t)
	port.worker.Config.PhoneMode = PhoneModeVoLTE
	b := &pendingNativeReadiness{entered: make(chan struct{}), release: make(chan struct{})}
	port.worker.Backend = b
	h := &nativeStartEffectHost{}
	p.volteCtl = volte.NewControllerWithBackup(h, t.TempDir())
	done := make(chan error, 1)
	go func() { done <- p.EnableNativeVoLTE(port.worker.ID) }()
	select {
	case <-b.entered:
	case <-time.After(time.Second):
		t.Fatal("startup did not reach readiness wait")
	}
	if err := p.stopNativeVoLTEForModemVoice(port.worker.ID); err != nil {
		t.Fatal(err)
	}
	applyTestCardPolicy(t, port.worker, cardpolicy.Policy{PhoneMode: PhoneModeModemVoice, VoWiFiEnabled: true})
	// Readiness must stop without waiting for the backend to become ready.
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stale start returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("handoff did not cancel readiness wait")
	}
	if h.starts.Load() != 0 || p.volteCtl.Status(port.worker.ID).Phase != volte.PhaseIdle {
		t.Fatal("canceled startup installed native IMS after handoff")
	}
}

func TestNativeStartOldCompletionCannotRemoveReplacement(t *testing.T) {
	p, port := testModemVoicePort(t)
	old := p.beginNativeVoLTESchedule(port.worker.ID)
	finish := p.beginNativeVoLTETransition(port.worker.ID)
	if old.ctx.Err() != context.Canceled {
		t.Fatal("handoff retained stale start")
	}
	port.worker.Config.PhoneMode = PhoneModeVoLTE
	next := p.beginNativeVoLTESchedule(port.worker.ID)
	if next == nil {
		t.Fatal("returning to native mode could not start")
	}
	defer p.endNativeVoLTESchedule(port.worker.ID, next)
	select {
	case <-next.ready:
		t.Fatal("queued start became runnable before handoff finished")
	default:
	}
	finish()
	p.endNativeVoLTESchedule(port.worker.ID, old)
	if err := p.validateNativeVoLTEStart(port.worker.ID, next); err != nil {
		t.Fatal("old completion invalidated replacement", err)
	}
	if p.beginNativeVoLTESchedule(port.worker.ID) != nil {
		t.Fatal("old completion lost the new task's deduplication")
	}
}

func TestNativeStartRechecksOwnershipBeforeProvisioning(t *testing.T) {
	for _, changed := range []string{"mode", "airplane", "disabled", "sim", "worker", "rf_lock"} {
		t.Run(changed, func(t *testing.T) {
			p, port := testModemVoicePort(t)
			port.worker.Config.PhoneMode = PhoneModeVoLTE
			start := p.beginNativeVoLTESchedule(port.worker.ID)
			defer p.endNativeVoLTESchedule(port.worker.ID, start)
			if err := p.validateNativeVoLTEStart(port.worker.ID, start); err != nil {
				t.Fatal(err)
			}
			switch changed {
			case "mode":
				port.worker.Config.PhoneMode = PhoneModeModemVoice
			case "airplane":
				port.worker.Config.AirplaneEnabled = true
			case "disabled":
				port.worker.Config.VoWiFiEnabled = false
			case "sim":
				port.worker.state.Identity.ICCID = "another-sim"
			case "worker":
				p.workers[port.worker.ID] = &Worker{ID: port.worker.ID}
			case "rf_lock":
				port.worker.state.Identity.IMSI = "234870000000001"
			}
			h := &nativeStartEffectHost{}
			ctl := volte.NewControllerWithBackup(h, t.TempDir())
			err := ctl.EnableChecked(start.ctx, volte.EnableRequest{DeviceID: port.worker.ID,
				Validate: func() error { return p.validateNativeVoLTEStart(port.worker.ID, start) }})
			if err == nil || h.starts.Load() != 0 || ctl.Status(port.worker.ID).Phase != volte.PhaseIdle {
				t.Fatal("changed ownership still installed a native session", err)
			}
		})
	}
}
