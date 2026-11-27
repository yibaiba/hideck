package vowifihost

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost"
)

type restoreRecordingAdapter struct {
	*runtimeReadinessAdapter
	mu       sync.Mutex
	smsCalls int
	rfCalls  int
}

func (a *restoreRecordingAdapter) RestoreSMSMode(string) {
	a.mu.Lock()
	a.smsCalls++
	a.mu.Unlock()
}

func (a *restoreRecordingAdapter) RestoreRadioAfterVoWiFi(string) error {
	a.mu.Lock()
	a.rfCalls++
	a.mu.Unlock()
	return nil
}

func (a *restoreRecordingAdapter) calls() (int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.smsCalls, a.rfCalls
}

// Maintainer #119 point 2: teardown waits for the old runner; a timeout is an
// error, not a successful exit, and the SIM/SMS/radio are not handed over.
func TestDisableDoesNotHandOverWhileOldRunnerIsStillRunning(t *testing.T) {
	m := NewManager()
	adapter := &restoreRecordingAdapter{runtimeReadinessAdapter: &runtimeReadinessAdapter{}}
	m.ConfigureAdapter(adapter)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	inst := runtimehost.StartWithRunnerForTest(context.Background(), "dev-1", func(ctx context.Context) error {
		<-ctx.Done()
		<-release
		return ctx.Err()
	})
	m.RuntimeStore().SetInstance("dev-1", inst)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	err := m.disableRuntime(ctx, "dev-1", "test", true, false)
	cancel()
	if !errors.Is(err, runtimehost.ErrRunnerNotExited) {
		t.Fatalf("disableRuntime err = %v, want ErrRunnerNotExited", err)
	}
	if m.RuntimeStore().Instance("dev-1") != inst {
		t.Fatal("instance was removed although its runner has not exited")
	}
	if sms, rf := adapter.calls(); sms != 0 || rf != 0 {
		t.Fatalf("restored SMS=%d radio=%d before the old runner exited", sms, rf)
	}

	// While it is still exiting, the device stays occupied: a new start is
	// refused instead of reported as already running.
	if m.Running("dev-1") || !m.Active("dev-1") {
		t.Fatalf("Running=%v Active=%v while the old runner is exiting, want false/true", m.Running("dev-1"), m.Active("dev-1"))
	}
	if err := m.enableRuntime(context.Background(), runtimeEnableRequest{DeviceID: "dev-1"}); err == nil {
		t.Fatal("enable succeeded while the old runner was still exiting")
	}

	close(release)
	waitFor(t, func() bool { return m.RuntimeStore().Instance("dev-1") == nil })
	if sms, rf := adapter.calls(); sms != 0 || rf != 0 {
		t.Fatalf("late exit restored SMS=%d radio=%d, want 0 and 0", sms, rf)
	}
	// Disabling again after the exit restores SMS mode and radio as main does
	// when no instance is left.
	if err := m.disableRuntime(context.Background(), "dev-1", "test", true, false); err != nil {
		t.Fatalf("second disable err = %v", err)
	}
	if sms, rf := adapter.calls(); sms != 1 || rf != 1 {
		t.Fatalf("second disable restored SMS=%d radio=%d, want 1 and 1", sms, rf)
	}
}

// A runtime that exited but failed to release a local resource is handed over
// and the failure is reported, not hidden.
func TestDisableReportsCleanupFailureAfterHandover(t *testing.T) {
	m := NewManager()
	adapter := &restoreRecordingAdapter{runtimeReadinessAdapter: &runtimeReadinessAdapter{}}
	m.ConfigureAdapter(adapter)
	inst := runtimehost.StartWithRunnerForTest(context.Background(), "dev-1", func(ctx context.Context) error {
		<-ctx.Done()
		return errors.Join(ctx.Err(), fmt.Errorf("%w: delete TUN: busy", runtimehost.ErrCleanupFailed))
	})
	m.RuntimeStore().SetInstance("dev-1", inst)

	err := m.disableRuntime(context.Background(), "dev-1", "test", true, false)
	if !IsCleanupOnlyError(err) {
		t.Fatalf("disableRuntime err = %v, want a cleanup-only error", err)
	}
	if m.RuntimeStore().Instance("dev-1") != nil {
		t.Fatal("exited instance was not removed")
	}
	if sms, rf := adapter.calls(); sms != 1 || rf != 1 {
		t.Fatalf("restored SMS=%d radio=%d, want 1 and 1", sms, rf)
	}
}

// Runtime failure release keeps the instance until its runner has exited, so a
// teardown arriving meanwhile still finds something to wait for.
func TestFailedRuntimeReleaseKeepsInstanceUntilRunnerExits(t *testing.T) {
	m := NewManager()
	m.ConfigureAdapter(&restoreRecordingAdapter{runtimeReadinessAdapter: &runtimeReadinessAdapter{}})
	release := make(chan struct{})
	inst := runtimehost.StartWithRunnerForTest(context.Background(), "dev-1", func(ctx context.Context) error {
		<-ctx.Done()
		<-release
		return ctx.Err()
	})
	m.RuntimeStore().SetInstance("dev-1", inst)

	done := make(chan struct{})
	go func() {
		m.releaseFailedRuntime("dev-1", inst, runtimehost.State{SessionState: "error", LastError: "transport lost"})
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	if m.RuntimeStore().Instance("dev-1") != inst {
		close(release)
		t.Fatal("failed instance was removed before its runner exited")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("failure release did not finish after the runner exited")
	}
	if m.RuntimeStore().Instance("dev-1") != nil {
		t.Fatal("failed instance was not removed after its runner exited")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Waiting behind a command whose old runner has not exited must still end at
// the caller's deadline instead of blocking it like a plain mutex.
func TestLifecycleSubmitWaitEndsAtCallerDeadline(t *testing.T) {
	c := NewLifecycleController()
	entered := make(chan struct{})
	release := make(chan struct{})
	c.SetRunForTest(func(ctx context.Context, cmd LifecycleCommand) error {
		if cmd.Reason == "first" {
			close(entered)
			<-release
		}
		return nil
	})
	go func() {
		_ = c.Submit(context.Background(), LifecycleCommand{DeviceID: "dev-1", Kind: LifecycleCommandEnable, Reason: "first"})
	}()
	<-entered
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- c.Submit(ctx, LifecycleCommand{DeviceID: "dev-1", Kind: LifecycleCommandDisable, Reason: "second"})
	}()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Submit err = %v, want deadline exceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Submit kept waiting past its deadline")
	}
}

// An idle device is not put through Disable, but a command still in flight
// (e.g. a restart waiting for the modem before it registers as starting) is
// cancelled and drained first.
func TestStopOrDrainCancelsInFlightCommandOnIdleDevice(t *testing.T) {
	c := NewManager()
	cancelled := make(chan struct{})
	entered := make(chan struct{})
	var kinds []string
	var mu sync.Mutex
	c.LifecycleControllerForTest().TestRun = func(ctx context.Context, cmd LifecycleCommand) error {
		mu.Lock()
		kinds = append(kinds, cmd.Kind.String())
		mu.Unlock()
		if cmd.Reason == "in_flight" {
			close(entered)
			<-ctx.Done()
			close(cancelled)
		}
		return nil
	}
	go func() {
		_ = c.SubmitLifecycle(context.Background(), LifecycleCommand{DeviceID: "dev-1", Kind: LifecycleCommandRestart, Reason: "in_flight"})
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.StopOrDrain(ctx, "dev-1", "test", false); err != nil {
		t.Fatalf("StopOrDrain err = %v", err)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("StopOrDrain returned without cancelling the in-flight command")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(kinds) != 1 {
		t.Fatalf("commands = %v, want only the in-flight one (no Disable for an idle device)", kinds)
	}
}

// A second terminal event for the same instance must not start a second
// release that invalidates the first one.
func TestFailedRuntimeReleaseIsClaimedOnce(t *testing.T) {
	m := NewManager()
	m.ConfigureAdapter(&restoreRecordingAdapter{runtimeReadinessAdapter: &runtimeReadinessAdapter{}})
	release := make(chan struct{})
	inst := runtimehost.StartWithRunnerForTest(context.Background(), "dev-1", func(ctx context.Context) error {
		<-ctx.Done()
		<-release
		return ctx.Err()
	})
	m.RuntimeStore().SetInstance("dev-1", inst)
	first := make(chan bool, 1)
	go func() {
		first <- m.releaseFailedRuntime("dev-1", inst, runtimehost.State{SessionState: "error"})
	}()
	waitFor(t, inst.Stopped)
	second := make(chan bool, 1)
	go func() {
		second <- m.releaseFailedRuntime("dev-1", inst, runtimehost.State{SessionState: "error"})
	}()
	select {
	case claimed := <-second:
		if claimed {
			close(release)
			t.Fatal("second release claimed an instance that is already being released")
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("second release entered the release instead of leaving it to the first")
	}
	close(release)
	if !<-first {
		t.Fatal("first release did not complete")
	}
}

func TestRecoverRefusesWhileOldRuntimeIsExiting(t *testing.T) {
	m := NewManager()
	m.ConfigureAdapter(&restoreRecordingAdapter{runtimeReadinessAdapter: &runtimeReadinessAdapter{}})
	release := make(chan struct{})
	defer close(release)
	inst := runtimehost.StartWithRunnerForTest(context.Background(), "dev-1", func(ctx context.Context) error {
		<-ctx.Done()
		<-release
		return ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	_ = inst.Stop(ctx)
	cancel()
	m.RuntimeStore().SetInstance("dev-1", inst)
	if err := m.recoverRuntime(context.Background(), "dev-1", "test", 0, ""); err == nil {
		t.Fatal("recover reported success while the old runtime was still exiting")
	}
}
