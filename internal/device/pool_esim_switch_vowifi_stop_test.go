package device

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost"
	"github.com/yibaiba/hideck/internal/backend"
	"github.com/yibaiba/hideck/internal/config"
)

// #119 point 2: a switch first stops the old VoWiFi task; if it has not exited,
// the switch is aborted with an error and neither preparation nor failure
// cleanup touches the radio while the old task may still use the modem.
func TestESIMSwitchAbortsWithoutRadioChangeWhileOldVoWiFiRunnerRuns(t *testing.T) {
	previous := voWiFiStopWaitTimeout
	voWiFiStopWaitTimeout = 100 * time.Millisecond
	defer func() { voWiFiStopWaitTimeout = previous }()

	p := NewPool(&config.Config{})
	defer p.cancel()
	be := &esimSwitchRestoreBackendStub{mode: backend.BackendQMI, getMode: backend.ModeOnline}
	w := &Worker{ID: "dev-1", Backend: be, Config: config.DeviceConfig{
		ID: "dev-1", ESIMSwitch: config.ESIMSwitchConfig{RadioCycle: true},
	}}
	p.workers[w.ID] = w

	release := make(chan struct{})
	defer close(release)
	inst := runtimehost.StartWithRunnerForTest(context.Background(), w.ID, func(ctx context.Context) error {
		<-ctx.Done()
		<-release
		return ctx.Err()
	})
	p.voWiFiHost().RuntimeStore().SetInstance(w.ID, inst)

	token, err := p.prepareESIMSwitch(w.ID, p.beginESIMSwitchForWorker(w.ID, "", w))
	if !errors.Is(err, ErrSwitchVoWiFiNotStopped) || !errors.Is(err, runtimehost.ErrRunnerNotExited) {
		t.Fatalf("prepareESIMSwitch err = %v, want ErrSwitchVoWiFiNotStopped wrapping ErrRunnerNotExited", err)
	}
	if token == 0 {
		t.Fatal("aborted switch must return its token so the failure callback can clear it")
	}
	p.handleESIMSwitchFailedWithError(w.ID, token, err)

	if len(be.setCalls) != 0 {
		t.Fatalf("radio operating mode changed to %v while the old VoWiFi runner was running", be.setCalls)
	}
	if p.IsESIMSwitching(w.ID) {
		t.Fatal("aborted switch left the device in the switching state")
	}
}
