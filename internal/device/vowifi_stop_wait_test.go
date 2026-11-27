package device

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/vowifihost"
)

// #119 point 2 "等待清理期间仍应保留取消/状态入口，避免换一层锁继续卡住": while a
// previous command still waits for the old task, disable, restart and the
// native voice handover return at their deadline.
func TestVoWiFiStopEntryPointsReturnAtDeadlineWhileOldTaskRuns(t *testing.T) {
	previous := voWiFiStopWaitTimeout
	voWiFiStopWaitTimeout = 100 * time.Millisecond
	defer func() { voWiFiStopWaitTimeout = previous }()

	p := NewPool(&config.Config{})
	defer p.cancel()
	for name, call := range map[string]func() error{
		"disable":           func() error { return p.DisableVoWiFi("dev-1") },
		"restart":           func() error { return p.RestartVoWiFi("dev-1") },
		"native_voice_stop": func() error { return p.stopSoftwareIMS("dev-1", false) },
	} {
		// The old task notices cancellation but keeps running, like a runner
		// still releasing the SIM: the caller must cancel it and then give up
		// at its deadline instead of waiting forever.
		entered := make(chan struct{})
		cancelled := make(chan struct{})
		release := make(chan struct{})
		p.voWiFiHost().LifecycleControllerForTest().TestRun = func(ctx context.Context, cmd vowifihost.LifecycleCommand) error {
			if cmd.Reason == "old_task" {
				close(entered)
				<-ctx.Done()
				close(cancelled)
				<-release
			}
			return nil
		}
		go func() {
			_ = p.voWiFiHost().SubmitLifecycle(context.Background(), vowifihost.LifecycleCommand{
				DeviceID: "dev-1", Kind: vowifihost.LifecycleCommandEnable, Reason: "old_task",
			})
		}()
		<-entered

		result := make(chan error, 1)
		go func() { result <- call() }()
		select {
		case err := <-result:
			if !errors.Is(err, vowifihost.ErrPreviousCommandRunning) {
				close(release)
				t.Fatalf("%s err = %v, want ErrPreviousCommandRunning", name, err)
			}
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatalf("%s kept waiting past its deadline", name)
		}
		select {
		case <-cancelled:
		default:
			close(release)
			t.Fatalf("%s returned without cancelling the old task", name)
		}
		close(release)
		if err := p.voWiFiHost().WaitLifecycleIdle(context.Background(), "dev-1"); err != nil {
			t.Fatal(err)
		}
	}
}

// An idle device is not put through Disable before a reboot (no SMS-mode or
// generation side effects that main did not have), and a stopped instance whose
// runtime is still exiting is not restarted after the reboot.
func TestStopVoWiFiBeforeRebootIdleAndStoppedInstance(t *testing.T) {
	p := NewPool(&config.Config{})
	defer p.cancel()
	var submitted []string
	p.voWiFiHost().LifecycleControllerForTest().TestRun = func(ctx context.Context, cmd vowifihost.LifecycleCommand) error {
		submitted = append(submitted, cmd.Kind.String())
		return nil
	}
	hadVoWiFi, err := p.StopVoWiFiBeforeReboot(context.Background(), "dev-1")
	if err != nil || hadVoWiFi || len(submitted) != 0 {
		t.Fatalf("idle device: hadVoWiFi=%v err=%v submitted=%v, want false/nil/none", hadVoWiFi, err, submitted)
	}

	release := make(chan struct{})
	defer close(release)
	inst := runtimehost.StartWithRunnerForTest(context.Background(), "dev-1", func(ctx context.Context) error {
		<-ctx.Done()
		<-release
		return ctx.Err()
	})
	stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	_ = inst.Stop(stopCtx)
	cancel()
	p.voWiFiHost().RuntimeStore().SetInstance("dev-1", inst)
	if hadVoWiFi, _ := p.StopVoWiFiBeforeReboot(context.Background(), "dev-1"); hadVoWiFi {
		t.Fatal("an instance already being stopped was counted as VoWiFi to restore after the reboot")
	}
}
