package device

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yibaiba/hideck/internal/esim"
	modemhost "github.com/yibaiba/hideck/internal/modemvoice/host"
	"github.com/yibaiba/hideck/internal/volte"
)

func TestRemoveWorkerRetiresNativeSession(t *testing.T) {
	p, port := testModemVoicePort(t)
	id := port.worker.ID
	p.volteCtl = volte.NewControllerWithBackup(&nativeStartEffectHost{}, t.TempDir())
	if err := p.volteCtl.Enable(context.Background(), id); err == nil {
		t.Fatal("expected failed native startup")
	}
	if err := p.RemoveWorker(id); err != nil {
		t.Fatal(err)
	}
	if got := p.volteCtl.Status(id); got.Phase != volte.PhaseIdle {
		t.Fatalf("removed worker retained native state: %+v", got)
	}
	p.mu.Lock()
	p.workers[id] = &Worker{ID: id, Config: port.worker.Config}
	p.mu.Unlock()
	if got := p.volteCtl.Status(id); got.Phase != volte.PhaseIdle {
		t.Fatalf("replacement inherited native state: %+v", got)
	}
}

func TestAbandonDeviceReturnsCleanupFailureAndCanRetry(t *testing.T) {
	p, port := testModemVoicePort(t)
	id := port.worker.ID
	want := errors.New("cleanup failed")
	var fail atomic.Bool
	fail.Store(true)
	p.modemVoiceCtl = modemhost.New(modemhost.Options{
		Prepare: func(context.Context, string) (*modemhost.Resources, error) {
			return &modemhost.Resources{Close: func(context.Context) error {
				if fail.Load() {
					return want
				}
				return nil
			}}, want
		},
	})
	p.modemVoiceCtl.Enable(p.Context(), id)
	t.Cleanup(func() { fail.Store(false); _ = p.stopModemVoice(id) })
	deadline := time.Now().Add(2 * time.Second)
	for p.modemVoiceCtl.DeviceStatus(id)["phase"] != "failed" {
		if time.Now().After(deadline) {
			t.Fatal("modem preparation did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	if err := p.AbandonDevice(id); !errors.Is(err, want) {
		t.Fatalf("cleanup error lost: %v", err)
	}
	if p.GetWorker(id) != port.worker {
		t.Fatal("failed cleanup removed the worker")
	}
	fail.Store(false)
	if err := p.AbandonDevice(id); err != nil {
		t.Fatal("cleanup retry", err)
	}
	if p.GetWorker(id) != nil {
		t.Fatal("retry retained worker")
	}
}

func TestAbandonDeviceInvalidatesActiveESIMOperation(t *testing.T) {
	p, port := testModemVoicePort(t)
	id := port.worker.ID
	p.beginESIMSwitch(id, "8944100000000000042")
	transition := p.policyTransitionFor(id)
	before := policySwitchEpoch(transition)

	if err := p.AbandonDevice(id); err != nil {
		t.Fatal(err)
	}
	if after := policySwitchEpoch(transition); after <= before {
		t.Fatalf("switch epoch=%d want greater than %d", after, before)
	}
	if p.IsESIMSwitching(id) {
		t.Fatal("abandoned device retained eSIM switch state")
	}
}

func TestRemovedWorkerCannotStartNewESIMSwitch(t *testing.T) {
	p, port := testModemVoicePort(t)
	id := port.worker.ID
	onBefore, onAfter, _, _, _ := p.newESIMSwitchCallbacksForWorker(id, port.worker)
	if err := p.AbandonDevice(id); err != nil {
		t.Fatal(err)
	}
	replacement := &Worker{ID: id, Config: port.worker.Config}
	p.AttachWorkerForTest(replacement)

	if token, _ := onBefore(esim.SwitchOperationEnableProfile, "8944100000000000043"); token != 0 {
		t.Fatalf("stale eSIM manager returned switch token %d", token)
	}
	if p.IsESIMSwitching(id) {
		t.Fatal("stale eSIM manager marked the replacement worker as switching")
	}
	newSwitch := p.beginESIMSwitch(id, "8944100000000000044")
	onAfter(0)
	if !p.IsESIMSwitching(id) {
		t.Fatal("stale eSIM completion cleared the replacement worker switch")
	}
	p.handleESIMSwitchFailed(id, newSwitch.SwitchToken)
}

func TestAutomationDeviceOwnerRejectsReplacementHardware(t *testing.T) {
	original := &Worker{ID: "wwan0"}
	original.Config.ModemIMEI = "860000000000001"
	original.Config.USBPath = "1-2.3"
	owner := captureAutomationDeviceOwner(original)

	sameHardware := &Worker{ID: "wwan0"}
	sameHardware.Config.ModemIMEI = original.Config.ModemIMEI
	sameHardware.Config.USBPath = "1-4.1"
	if !owner.matches(sameHardware) {
		t.Fatal("same modem identity was rejected after re-enumeration")
	}

	replacement := &Worker{ID: "wwan0"}
	replacement.Config.ModemIMEI = "860000000000002"
	replacement.Config.USBPath = original.Config.USBPath
	if owner.matches(replacement) {
		t.Fatal("replacement modem inherited the active automatic task")
	}
}
