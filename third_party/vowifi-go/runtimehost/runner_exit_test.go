package runtimehost

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/iniwex5/vowifi-go/engine/swu"
	"github.com/iniwex5/vowifi-go/internal/vowifi/runtimecore"
)

// lingeringRunner keeps running after cancellation until release is closed,
// like a runner still cleaning up the SIM channel and network stack.
func lingeringRunner(release <-chan struct{}) coreRunner {
	return func(ctx context.Context, _ runtimecore.RuntimeStartRequest) (StartResult, error) {
		<-ctx.Done()
		<-release
		return StartResult{}, ctx.Err()
	}
}

func TestInstanceStopReportsRunnerThatHasNotExited(t *testing.T) {
	release := make(chan struct{})
	instance, _, _ := launchRuntimeCore(context.Background(), runtimecore.RuntimeStartRequest{DeviceID: "dev-1"},
		runtimeLaunchOptions{runner: lingeringRunner(release)})

	for attempt := 1; attempt <= 2; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		err := instance.Stop(ctx)
		cancel()
		if !errors.Is(err, ErrRunnerNotExited) {
			t.Fatalf("Stop attempt %d err = %v, want ErrRunnerNotExited", attempt, err)
		}
	}

	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := instance.Stop(ctx); err != nil {
		t.Fatalf("Stop after runner exit err = %v, want nil", err)
	}
}

func TestStartInstanceCancelReturnsOnlyAfterRunnerExit(t *testing.T) {
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() {
		_, err := startInstance(ctx, runtimecore.RuntimeStartRequest{DeviceID: "dev-1"},
			runtimeLaunchOptions{runner: lingeringRunner(release)})
		returned <- err
	}()

	cancel()
	select {
	case err := <-returned:
		t.Fatalf("startInstance returned %v while the runner was still running", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("startInstance err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("startInstance did not return after the runner exited")
	}
}

// #119 point 2: a caller's deadline holds even while the release task is stuck
// in the IMS stop, and every later Stop gets the same final result.
func TestInstanceStopReturnsAtDeadlineWhileReleaseIsBlocked(t *testing.T) {
	release := make(chan struct{})
	instance := &Instance{service: &recordingStopService{onStop: func() { <-release }}}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- instance.Stop(ctx) }()
	select {
	case err := <-result:
		if !errors.Is(err, ErrRunnerNotExited) {
			t.Fatalf("Stop err = %v, want ErrRunnerNotExited", err)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("Stop kept waiting past its deadline")
	}
	if !instance.Stopped() {
		t.Fatal("Stopped() = false after Stop")
	}

	close(release)
	if err := instance.Stop(context.Background()); err != nil {
		t.Fatalf("Stop after release err = %v, want nil", err)
	}
}

// Local cleanup failures of an exited runner reach the host as ErrCleanupFailed.
func TestInstanceStopReportsRunnerCleanupFailure(t *testing.T) {
	cleanupErr := fmt.Errorf("%w: delete TUN: busy", swu.ErrSessionCleanup)
	instance, _, _ := launchRuntimeCore(context.Background(), runtimecore.RuntimeStartRequest{DeviceID: "dev-1"},
		runtimeLaunchOptions{runner: func(ctx context.Context, _ runtimecore.RuntimeStartRequest) (StartResult, error) {
			<-ctx.Done()
			return StartResult{}, errors.Join(ctx.Err(), cleanupErr)
		}})
	err := instance.Stop(context.Background())
	if !errors.Is(err, ErrCleanupFailed) || errors.Is(err, ErrRunnerNotExited) {
		t.Fatalf("Stop err = %v, want ErrCleanupFailed only", err)
	}
}

// A late ims_registered/ipsec_up after Stop must not reattach handles that the
// release task will never stop.
func TestStoppedInstanceIgnoresLateHandles(t *testing.T) {
	instance := &Instance{}
	if err := instance.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	instance.setService(&recordingStopService{})
	instance.setSession(&runtimecore.SessionResult{})
	if instance.Service() != nil || instance.session != nil {
		t.Fatal("stopped instance accepted a late service or session")
	}
}

// A voice detach attached after Stop runs at once instead of being kept for a
// release task that already ran.
func TestStoppedInstanceRunsLateVoiceDetach(t *testing.T) {
	instance := &Instance{}
	if err := instance.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	detached := false
	instance.setVoiceDetach(func() error { detached = true; return nil })
	if !detached {
		t.Fatal("late voice detach was not run")
	}
}
