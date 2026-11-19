package device

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yibaiba/hideck/internal/cardpolicy"
	"github.com/yibaiba/hideck/internal/volte"
)

type handoffATExecutor func(context.Context, string, time.Duration) (string, error)

func (f handoffATExecutor) ExecuteATContext(ctx context.Context, cmd string, timeout time.Duration) (string, error) {
	return f(ctx, cmd, timeout)
}

func newTestIdleHandoff(t *testing.T) (*Pool, *nativeIdleHandoff) {
	t.Helper()
	p, port := testModemVoicePort(t)
	port.worker.Config.PhoneMode = PhoneModeVoLTE
	port.worker.Backend = &workerStatusBackendStub{mode: "at"}
	start := &nativeVoLTEStart{worker: port.worker, sim: currentSIMOwnership(port.worker)}
	binding, ok := p.nativeIdleHandoff(start).(*nativeIdleHandoff)
	if !ok {
		t.Fatal("AT worker did not bind idle handoff")
	}
	return p, binding
}

func TestNativeIdleHandoffCLCCResults(t *testing.T) {
	for _, response := range []string{
		"OK", "+CLCC: 1,0,0,0,0\r\nOK", "+CLCC: 1,0,0,1,0\r\nOK",
		"+CLCC: 1,1,4,9,0\r\nOK", "ERROR", "+CLCC: broken\r\nOK",
		"+CLCC: 1,0,0,0,0\r\n+CME ERROR: 100",
	} {
		t.Run(response, func(t *testing.T) {
			_, binding := newTestIdleHandoff(t)
			binding.executor = handoffATExecutor(func(_ context.Context, command string, _ time.Duration) (string, error) {
				if command != "AT+CLCC" {
					t.Fatalf("unexpected command %s", command)
				}
				return response, nil
			})
			err := binding.CheckIdle(context.Background())
			if (err == nil) != (response == "OK") {
				t.Fatalf("response %q: %v", response, err)
			}
		})
	}
}

func TestNativeIdleHandoffRejectsChangedOwner(t *testing.T) {
	for _, change := range []string{"worker", "sim", "backend", "cancel"} {
		for _, during := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/during=%t", change, during), func(t *testing.T) {
				p, binding := newTestIdleHandoff(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				mutate := func() {
					switch change {
					case "worker":
						p.workers[binding.port.worker.ID] = &Worker{}
					case "sim":
						binding.port.worker.state.Identity.ICCID = "other"
					case "backend":
						binding.port.worker.Backend = &workerStatusBackendStub{mode: "qmi"}
					case "cancel":
						cancel()
					}
				}
				binding.executor = handoffATExecutor(func(context.Context, string, time.Duration) (string, error) {
					if !during {
						t.Fatal("query touched changed owner")
					}
					mutate()
					return "OK", nil
				})
				if !during {
					mutate()
				}
				if err := binding.CheckIdle(ctx); err == nil {
					t.Fatal("changed owner allowed handoff")
				}
			})
		}
	}
}

func TestNativeIdleHandoffRetainsTransportErrors(t *testing.T) {
	_, binding := newTestIdleHandoff(t)
	for _, failure := range []error{context.DeadlineExceeded, context.Canceled, errors.New("AT disconnected")} {
		binding.executor = handoffATExecutor(func(context.Context, string, time.Duration) (string, error) {
			return "", failure
		})
		if err := binding.CheckIdle(context.Background()); !errors.Is(err, failure) {
			t.Fatal(err)
		}
	}
}

func TestNativeIdleHandoffRetriesRejectedSnapshotAndHonorsCancellation(t *testing.T) {
	for _, cancelRetry := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelRetry), func(t *testing.T) {
			_, binding := newTestIdleHandoff(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			queries := 0
			binding.executor = handoffATExecutor(func(context.Context, string, time.Duration) (string, error) {
				queries++
				if queries == 1 {
					if cancelRetry {
						cancel()
					}
					return "+CLCC: 1,0,0,0,0\r\n+CME ERROR: 100", nil
				}
				return "OK", nil
			})
			err := binding.CheckIdle(ctx)
			if cancelRetry {
				if !errors.Is(err, context.Canceled) || queries != 1 {
					t.Fatal("retry ignored cancellation", err)
				}
			} else if err != nil || queries != 2 {
				t.Fatal("snapshot was not rechecked", err)
			}
		})
	}
}

func TestFailedATNativeInitializationAllowsPolicyHandoff(t *testing.T) {
	p, binding := newTestIdleHandoff(t)
	w := binding.port.worker
	checks := 0
	binding.executor = handoffATExecutor(func(context.Context, string, time.Duration) (string, error) {
		checks++
		return "OK", nil
	})
	h := &idlePolicyHost{failingHandoffHost: failingHandoffHost{failure: errors.New("initialization failed")}}
	p.volteCtl = volte.NewControllerWithBackup(h, t.TempDir())
	err := p.volteCtl.EnableChecked(p.Context(), volte.EnableRequest{DeviceID: w.ID, IdleHandoff: binding})
	if !errors.Is(err, h.failure) {
		t.Fatal(err)
	}
	p.SetPolicyResolver(&stubPolicyResolver{pol: cardpolicy.Policy{
		ICCID: binding.port.iccid, PhoneMode: PhoneModeModemVoice, VoWiFiEnabled: true,
	}})
	result := p.resolveAndApplyPolicy(w, "test_at_handoff")
	if !result.Applied || w.Config.PhoneMode != PhoneModeModemVoice || checks != 1 {
		t.Fatalf("handoff failed: %+v checks=%d", result, checks)
	}
	if p.volteCtl.Status(w.ID).Phase != volte.PhaseIdle {
		t.Fatal("old session remains")
	}
}

type idlePolicyHost struct{ failingHandoffHost }

func (*idlePolicyHost) ReleaseIMSClients(string) error { return nil }
