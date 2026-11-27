package esim

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/damonto/euicc-go/lpa"
)

// #119 point 2: when the pre-switch hook cannot stop the old VoWiFi task, the
// profile APDU is not sent and the failure callback gets the hook's error.
func TestProfileOperationsAbortBeforeAPDUWhenSwitchPreparationFails(t *testing.T) {
	const aidHex = "A0000005591010FFFFFFFF8900000100"
	const targetICCID = "8986001234567890123"
	prepareErr := errors.New("old VoWiFi task still running")

	for _, tc := range []struct {
		name string
		run  func(*Manager) error
	}{
		{"enable", func(m *Manager) error {
			_, err := m.SwitchProfileWithResult(context.Background(), targetICCID, aidHex)
			return err
		}},
		{"disable", func(m *Manager) error {
			return m.DisableProfile(context.Background(), targetICCID, aidHex)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var profileCalls, channelOpens atomic.Int32
			mgr := newTestQMIManagerForPowerCycle(t, &fakeSIMPowerBackend{}, &profileCalls)
			openChannel := mgr.channelFactory
			mgr.channelFactory = func(aid []byte) (*lpa.Client, error) {
				channelOpens.Add(1)
				return openChannel(aid)
			}
			mgr.onBeforeSwitch = func(SwitchOperation, string) (uint64, error) {
				return 7, prepareErr
			}
			var failedToken uint64
			var failedErr error
			mgr.onSwitchFailed = func(_ SwitchOperation, token uint64, err error) {
				failedToken, failedErr = token, err
			}

			if err := tc.run(mgr); !errors.Is(err, prepareErr) {
				t.Fatalf("operation err = %v, want wrapping %v", err, prepareErr)
			}
			if profileCalls.Load() != 0 || channelOpens.Load() != 0 {
				t.Fatalf("profile APDU calls=%d LPA channel opens=%d, want 0 and 0: the SIM was accessed before the old task stopped",
					profileCalls.Load(), channelOpens.Load())
			}
			if failedToken != 7 || !errors.Is(failedErr, prepareErr) {
				t.Fatalf("failed callback token=%d err=%v, want 7 and %v", failedToken, failedErr, prepareErr)
			}
		})
	}
}
