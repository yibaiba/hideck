package device

import (
	"context"
	"testing"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost"
	"github.com/yibaiba/hideck/internal/cardpolicy"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/db"
	"github.com/yibaiba/hideck/internal/modem"
	"github.com/yibaiba/hideck/internal/vowifihost"
)

func TestPhysicalSIMInsertEventRefreshesIdentity(t *testing.T) {
	p := NewPool(&config.Config{})
	t.Cleanup(func() { _ = p.Shutdown() })
	deviceBackend := &workerStartupIdentityBackendStub{liveICCID: "card-new", liveIMSI: "imsi-new"}
	deviceBackend.simInserted = true
	w := &Worker{
		ID:      "dev",
		Pool:    p,
		Backend: deviceBackend,
		Config:  config.DeviceConfig{VoWiFiEnabled: true, PhoneMode: PhoneModeWiFi},
	}
	setWorkerSMSIdentity(w, SMSIdentity{ICCID: "card-old", IMSI: "imsi-old"})
	p.AttachWorkerForTest(w)
	p.SetPolicyResolver(&stubPolicyResolver{pol: cardpolicy.Policy{
		ICCID: "card-new", VoWiFiEnabled: true, PhoneMode: PhoneModeWiFi,
	}})

	p.confirmSIMRemovedAndStopVoWiFi(w.ID, "test")
	if got := w.CurrentICCID(); got != "card-new" {
		t.Fatalf("ICCID=%q want card-new", got)
	}
	if got := w.GetCachedIMSI(); got != "imsi-new" {
		t.Fatalf("IMSI=%q want imsi-new", got)
	}
	p.deviceEventWakeMu.Lock()
	_, recoveryScheduled := p.deviceEventWakeups[w.ID]
	p.deviceEventWakeMu.Unlock()
	if !recoveryScheduled {
		t.Fatal("refreshed SIM did not schedule desired VoWiFi recovery")
	}
}

func TestStaleSIMConfirmationKeepsCurrentWorkerTimer(t *testing.T) {
	p := NewPool(&config.Config{})
	t.Cleanup(func() { _ = p.Shutdown() })
	w := &Worker{ID: "dev", Pool: p}
	p.AttachWorkerForTest(w)

	timer := time.NewTimer(time.Hour)
	t.Cleanup(func() { timer.Stop() })
	p.simEventMu.Lock()
	p.simEventTimers = map[string]*time.Timer{w.ID: timer}
	p.simEventMu.Unlock()

	old := workerLifecycleOwner{worker: &Worker{ID: w.ID, Pool: p}}
	oldTimer := time.NewTimer(time.Hour)
	t.Cleanup(func() { oldTimer.Stop() })
	p.confirmSIMRemovedForOwner(old, "at_startup", nil)
	p.confirmSIMRemovedForOwner(old, "sim_event", oldTimer)

	p.simEventMu.Lock()
	current := p.simEventTimers[w.ID]
	p.simEventTimers[w.ID] = oldTimer
	p.simEventMu.Unlock()
	if current != timer {
		t.Fatal("stale worker consumed the current worker's SIM status timer")
	}

	// An expired callback can still consume the exact timer it was scheduled for.
	p.confirmSIMRemovedForOwner(old, "sim_event", oldTimer)
	p.simEventMu.Lock()
	current = p.simEventTimers[w.ID]
	p.simEventMu.Unlock()
	if current != nil {
		t.Fatal("expired callback left its own timer registered")
	}
}

func TestConfirmedSIMRemovalClearsIdentityAndDatabaseBinding(t *testing.T) {
	initDevicePhoneNumberTestDB(t)
	p := NewPool(&config.Config{})
	t.Cleanup(func() { _ = p.Shutdown() })
	const imei = "860000000000333"
	if err := db.UpsertDevice(imei, "dev", "model", "port"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSIMCardIdentity("card-old", "imsi-old", "", ptrString(imei)); err != nil {
		t.Fatal(err)
	}
	deviceBackend := &workerStartupIdentityBackendStub{}
	deviceBackend.simInserted = false
	w := &Worker{ID: "dev", Pool: p, Backend: deviceBackend, Config: config.DeviceConfig{ModemIMEI: imei}}
	w.state.Identity.IMEI = imei
	setWorkerSMSIdentity(w, SMSIdentity{ICCID: "card-old", IMSI: "imsi-old"})
	p.AttachWorkerForTest(w)
	before := w.SIMIdentityGeneration()

	p.confirmSIMRemovedAndStopVoWiFi(w.ID, "test")
	if got := w.CurrentICCID(); got != "" {
		t.Fatalf("runtime retained removed SIM %q", got)
	}
	if w.SIMIdentityGeneration() <= before {
		t.Fatal("SIM removal did not retire the previous identity generation")
	}
	if got := db.CurrentICCIDForDevice(w.ID); got != "" {
		t.Fatalf("database retained removed SIM %q", got)
	}
}

func TestPhysicalSIMSwapStopsOldVoWiFiBeforeApplyingNewPolicy(t *testing.T) {
	p := NewPool(&config.Config{})
	t.Cleanup(func() { _ = p.Shutdown() })
	deviceBackend := &workerStartupIdentityBackendStub{liveICCID: "card-new", liveIMSI: "imsi-new"}
	deviceBackend.simInserted = true
	w := &Worker{
		ID:      "dev",
		Pool:    p,
		Backend: deviceBackend,
		Config:  config.DeviceConfig{VoWiFiEnabled: true, PhoneMode: PhoneModeWiFi},
	}
	setWorkerSMSIdentity(w, SMSIdentity{ICCID: "card-old", IMSI: "imsi-old"})
	p.AttachWorkerForTest(w)
	p.SetPolicyResolver(&stubPolicyResolver{pol: cardpolicy.Policy{
		ICCID: "card-new", VoWiFiEnabled: true, PhoneMode: PhoneModeWiFi,
	}})
	p.voWiFiRuntimeStore().SetInstance(w.ID, &runtimehost.Instance{})
	t.Cleanup(func() { p.voWiFiRuntimeStore().DeleteInstance(w.ID, nil) })
	commands := make(chan vowifihost.LifecycleCommand, 1)
	p.voWiFiHost().LifecycleControllerForTest().TestRun = func(_ context.Context, command vowifihost.LifecycleCommand) error {
		commands <- command
		return nil
	}

	p.confirmSIMRemovedAndStopVoWiFi(w.ID, "test")
	select {
	case command := <-commands:
		if command.Kind != vowifihost.LifecycleCommandDisable || command.Reason != "sim_identity_changed" {
			t.Fatalf("lifecycle command=%s reason=%q want disable/sim_identity_changed", command.Kind, command.Reason)
		}
	default:
		t.Fatal("physical SIM swap left the previous VoWiFi runtime active")
	}
}

func TestConfirmedSIMRemovalCancelsStartingVoWiFi(t *testing.T) {
	p := NewPool(&config.Config{})
	t.Cleanup(func() { _ = p.Shutdown() })
	deviceBackend := &workerStartupIdentityBackendStub{}
	deviceBackend.simInserted = false
	w := &Worker{ID: "dev", Pool: p, Backend: deviceBackend}
	setWorkerSMSIdentity(w, SMSIdentity{ICCID: "card-old", IMSI: "imsi-old"})
	p.AttachWorkerForTest(w)
	if claim := p.voWiFiHost().BeginStart(w.ID); !claim.Accepted {
		t.Fatalf("BeginStart()=%+v want accepted", claim)
	}
	commands := make(chan vowifihost.LifecycleCommand, 1)
	p.voWiFiHost().LifecycleControllerForTest().TestRun = func(_ context.Context, command vowifihost.LifecycleCommand) error {
		commands <- command
		return nil
	}

	p.confirmSIMRemovedAndStopVoWiFi(w.ID, "test")
	select {
	case command := <-commands:
		if command.Kind != vowifihost.LifecycleCommandDisable || command.Reason != "sim_removed" {
			t.Fatalf("lifecycle command=%s reason=%q want disable/sim_removed", command.Kind, command.Reason)
		}
	default:
		t.Fatal("SIM removal did not cancel the in-progress VoWiFi start")
	}
}

type ml307cSIMBackendStub struct {
	workerStartupIdentityBackendStub
	simErr     error
	onSIMQuery func()
}

func (s *ml307cSIMBackendStub) IsSimInserted(context.Context) (bool, error) {
	if s.onSIMQuery != nil {
		s.onSIMQuery()
	}
	return s.simErr == nil, s.simErr
}

func confirmScheduledSIMPIN(t *testing.T, p *Pool, owner workerLifecycleOwner) {
	t.Helper()
	p.simEventMu.Lock()
	timer := p.simEventTimers[owner.worker.ID]
	p.simEventMu.Unlock()
	if timer == nil {
		t.Fatal("PIN event did not schedule a SIM status check")
	}
	if !timer.Stop() {
		t.Fatal("SIM status check ran before the test could confirm it")
	}
	p.confirmSIMStatusForOwner(owner, "test", timer, true)
}

func TestML307CDelayedPINKeepsCurrentSIMAndCall(t *testing.T) {
	p := NewPool(&config.Config{})
	t.Cleanup(func() { _ = p.Shutdown() })
	cfg := config.DeviceConfig{ID: "c", DeviceBackend: "at", ATPort: "/dev/ttyUSB8"}
	m, err := modem.NewWithATProfile(cfg, modem.ATProfileML307C)
	if err != nil {
		t.Fatal(err)
	}
	be := &ml307cSIMBackendStub{}
	be.liveICCID, be.liveIMSI = "card-new", "imsi-new"
	w := &Worker{ID: cfg.ID, Pool: p, Config: cfg, Modem: m, Backend: be}
	p.AttachWorkerForTest(w)
	setWorkerSMSIdentity(w, SMSIdentity{ICCID: "card-old", IMSI: "imsi-old"})
	w.state.Identity.Generation++
	setWorkerSMSIdentity(w, SMSIdentity{ICCID: "card-new", IMSI: "imsi-new"})
	w.state.Identity.Generation++
	before := currentSIMOwnership(w)
	owner := p.captureWorkerLifecycleOwner(w.ID)

	instance := &runtimehost.Instance{}
	p.voWiFiRuntimeStore().SetInstance(w.ID, instance)
	t.Cleanup(func() { p.voWiFiRuntimeStore().DeleteInstance(w.ID, nil) })
	stops := 0
	p.voWiFiHost().LifecycleControllerForTest().TestRun = func(_ context.Context, _ vowifihost.LifecycleCommand) error {
		stops++
		return nil
	}

	p.handleSIMStatusEventForOwner(owner, "test", nil, "SIM PIN")
	confirmScheduledSIMPIN(t, p, owner)
	if got := currentSIMOwnership(w); got != before || w.SIMIdentityConvergenceMatches("", 0) {
		t.Fatalf("stale PIN changed ready SIM ownership: before=%+v after=%+v", before, got)
	}
	if stops != 0 {
		t.Fatalf("stale PIN stopped an active runtime %d times", stops)
	}

	p.switchMu.Lock()
	p.switchingDevices[w.ID] = true
	p.switchMu.Unlock()
	be.simErr = modem.ErrSIMPINRequired
	p.handleSIMStatusEventForOwner(owner, "test", nil, "SIM PIN")
	confirmScheduledSIMPIN(t, p, owner)
	if got := currentSIMOwnership(w); got != before || stops != 0 {
		t.Fatalf("PIN during eSIM switch changed SIM ownership or stopped calls: before=%+v after=%+v stops=%d", before, got, stops)
	}
	p.switchMu.Lock()
	delete(p.switchingDevices, w.ID)
	p.switchMu.Unlock()

	// A separate live identity refresh can publish a new generation while
	// the PIN confirmation reads the backend.
	be.onSIMQuery = func() {
		w.cacheMu.Lock()
		defer w.cacheMu.Unlock()
		setWorkerSMSIdentity(w, SMSIdentity{ICCID: "card-newer", IMSI: "imsi-newer"})
		w.state.Identity.Generation++
	}
	p.handleSIMStatusEventForOwner(owner, "test", nil, "SIM PIN")
	confirmScheduledSIMPIN(t, p, owner)
	if got := currentSIMOwnership(w); got.iccid != "card-newer" || got.generation != before.generation+1 ||
		w.SIMIdentityConvergenceMatches("", 0) || stops != 0 {
		t.Fatalf("old PIN result affected a newer SIM generation: ownership=%+v stops=%d", got, stops)
	}
}

func TestML307CPINRecoveryRetriesIdentityOnRepeatedReady(t *testing.T) {
	p := NewPool(&config.Config{})
	t.Cleanup(func() { _ = p.Shutdown() })
	cfg := config.DeviceConfig{ID: "c", DeviceBackend: "at", ATPort: "/dev/ttyUSB8"}
	m, err := modem.NewWithATProfile(cfg, modem.ATProfileML307C)
	if err != nil {
		t.Fatal(err)
	}
	be := &ml307cSIMBackendStub{simErr: modem.ErrSIMPINRequired}
	w := &Worker{ID: cfg.ID, Pool: p, Config: cfg, Modem: m, Backend: be}
	setWorkerSMSIdentity(w, SMSIdentity{ICCID: "card-old", IMSI: "imsi-old"})
	p.AttachWorkerForTest(w)
	owner := p.captureWorkerLifecycleOwner(w.ID)
	t.Cleanup(func() { p.cancelWorkerLifecycleTimers(w.ID) })

	p.voWiFiRuntimeStore().SetInstance(w.ID, &runtimehost.Instance{})
	t.Cleanup(func() { p.voWiFiRuntimeStore().DeleteInstance(w.ID, nil) })
	commands := make(chan vowifihost.LifecycleCommand, 1)
	p.voWiFiHost().LifecycleControllerForTest().TestRun = func(_ context.Context, command vowifihost.LifecycleCommand) error {
		commands <- command
		return nil
	}

	p.handleSIMStatusEventForOwner(owner, "test", nil, "SIM PUK")
	confirmScheduledSIMPIN(t, p, owner)
	select {
	case command := <-commands:
		if command.Kind != vowifihost.LifecycleCommandDisable {
			t.Fatalf("PIN lock lifecycle command = %s", command.Kind)
		}
	default:
		t.Fatal("PIN lock left the old VoWiFi runtime active")
	}
	p.voWiFiRuntimeStore().DeleteInstance(w.ID, nil)
	if !w.SIMIdentityConvergenceMatches("", 0) || w.CurrentICCID() != "card-old" {
		t.Fatal("PIN lock must block the old identity and preserve its binding")
	}
	if err := p.refreshModemRebootRecoveredIdentity(w, "manual_reboot"); err != nil {
		t.Fatalf("PIN lock must allow control-plane recovery: %v", err)
	}

	be.simErr = nil
	ready := true
	confirmPoll := func() {
		t.Helper()
		p.handleSIMStatusEventForOwner(owner, "test", &ready, "")
		p.simEventMu.Lock()
		timer := p.simEventTimers[w.ID]
		p.simEventMu.Unlock()
		if timer == nil {
			t.Fatal("READY did not schedule SIM identity convergence")
		}
		timer.Stop()
		p.confirmSIMRemovedForOwner(owner, "test", timer)
	}

	confirmPoll() // CPIN says READY, but the first live ICCID/IMSI read is empty.
	if !w.SIMIdentityConvergenceMatches("", 0) || w.CurrentICCID() != "card-old" {
		t.Fatal("failed identity refresh must leave SIM convergence pending")
	}

	be.liveICCID, be.liveIMSI = "card-new", "imsi-new"
	confirmPoll()
	if w.SIMIdentityConvergenceMatches("", 0) || w.CurrentICCID() != "card-new" {
		t.Fatalf("repeated READY did not converge to the fresh ICCID: %q", w.CurrentICCID())
	}
}

func TestConfirmedSIMInsertionWaitsForActiveMBNPolicy(t *testing.T) {
	p := NewPool(&config.Config{})
	t.Cleanup(func() { _ = p.Shutdown() })
	w := &Worker{ID: "dev", Pool: p}
	setWorkerSMSIdentity(w, SMSIdentity{ICCID: "card-new", IMSI: "imsi-new"})
	p.AttachWorkerForTest(w)
	transition := p.policyTransitionFor(w.ID)
	transition.mbnSession.Store(&mbnDeviceSession{})
	t.Cleanup(func() { transition.mbnSession.Store(nil) })

	p.finishConfirmedSIMInsertion(p.captureWorkerLifecycleOwner(w.ID), "test", simInsertionRefreshResult{
		current: true, identityChanged: true,
	})
	p.deviceEventWakeMu.Lock()
	_, recoveryScheduled := p.deviceEventWakeups[w.ID]
	p.deviceEventWakeMu.Unlock()
	if recoveryScheduled {
		t.Fatal("SIM insertion scheduled recovery before the MBN policy operation completed")
	}
}
