package vowifihost

import (
	"context"
	"testing"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost"
	"github.com/iniwex5/vowifi-go/runtimehost/identity"
)

type runtimeStartTestModem struct{}

func (runtimeStartTestModem) DeviceID() string { return "dev-1" }
func (runtimeStartTestModem) IsHealthy() bool  { return true }
func (runtimeStartTestModem) IsSimInserted() bool {
	return true
}
func (runtimeStartTestModem) QuerySIMInserted() (bool, error) { return true, nil }
func (runtimeStartTestModem) GetRegStatus() (int, string)     { return 1, "registered" }
func (runtimeStartTestModem) GetNetworkMode() string          { return "LTE" }
func (runtimeStartTestModem) Capabilities() runtimehost.ModemCapabilities {
	return runtimehost.ModemCapabilities{}
}
func (runtimeStartTestModem) IMSIdentityProvider() runtimehost.IMSIdentityProvider {
	return runtimeStartTestModem{}
}
func (runtimeStartTestModem) GetISIMIdentity() (identity.Identity, error) {
	return identity.Identity{IMPI: "310260123456789@ims.example.com", IMPU: []string{"sip:310260123456789@ims.example.com"}, Domain: "ims.example.com"}, nil
}
func (runtimeStartTestModem) ExecuteATSilent(string, time.Duration) (string, error) {
	return "", nil
}
func (runtimeStartTestModem) OpenLogicalChannel(string) (int, error) { return 0, nil }
func (runtimeStartTestModem) CloseLogicalChannel(int) error          { return nil }
func (runtimeStartTestModem) TransmitAPDU(int, string) (string, error) {
	return "", nil
}
func (runtimeStartTestModem) Stop() {}

func TestManagerStartRuntimeBuildsRequestAndClaimsInstance(t *testing.T) {
	manager := NewManager()
	deviceID := "dev-1"
	claim := manager.BeginStart(deviceID)
	if !claim.Accepted {
		t.Fatalf("BeginStart() = %+v, want accepted", claim)
	}
	wantInst := &runtimehost.Instance{}
	var captured runtimehost.StartRequest
	manager.SetRuntimeStartForTest(func(ctx context.Context, req runtimehost.StartRequest) (*runtimehost.Instance, error) {
		captured = req
		if !req.ShouldRun() {
			t.Fatal("StartRequest.ShouldRun() = false before invalidation, want true")
		}
		return wantInst, nil
	})

	result, err := manager.StartRuntime(context.Background(), RuntimeStartRequest{
		DeviceID: deviceID,
		TraceID:  "trace-1",
		Epoch:    claim.Epoch,
		Prepared: PreparedStart{
			Profile: identity.Profile{IMSI: "001010000000001"},
			SIM:     runtimehost.NewReaderSIMAdapter(simProviderStub{}),
			Prepared: identity.PreparedSession{
				Profile: identity.Profile{IMSI: "001010000000001"},
			},
			NetworkMode: "LTE",
		},
		Modem:     runtimeStartTestModem{},
		Dataplane: runtimehost.DataplanePolicy{Mode: "userspace"},
	})
	if err != nil {
		t.Fatalf("StartRuntime() error = %v", err)
	}
	if result.Instance != wantInst || result.Stale {
		t.Fatalf("StartRuntime() = %+v, want claimed instance", result)
	}
	if manager.Instance(deviceID) != wantInst {
		t.Fatal("StartRuntime() should claim instance in runtime store")
	}
	if captured.Mode != runtimehost.StartModeMain || captured.DeviceID != deviceID || captured.TraceID != "trace-1" {
		t.Fatalf("captured request identity = mode %q device %q trace %q", captured.Mode, captured.DeviceID, captured.TraceID)
	}
	if captured.SIM == nil || captured.Access == nil {
		t.Fatal("captured request should include SIM and Access adapters")
	}
	if captured.NetworkMode != "LTE" || captured.Dataplane.Mode != "userspace" {
		t.Fatalf("captured request network/dataplane = %q/%q", captured.NetworkMode, captured.Dataplane.Mode)
	}
}

func TestManagerStartRuntimePreservesReaderMode(t *testing.T) {
	manager := NewManager()
	claim := manager.BeginStart("reader-1")
	var captured runtimehost.StartRequest
	manager.SetRuntimeStartForTest(func(_ context.Context, req runtimehost.StartRequest) (*runtimehost.Instance, error) {
		captured = req
		return &runtimehost.Instance{}, nil
	})
	_, err := manager.StartRuntime(context.Background(), RuntimeStartRequest{
		DeviceID: "reader-1", Epoch: claim.Epoch,
		Prepared: PreparedStart{
			Mode:     runtimehost.StartModeReader,
			SIM:      runtimehost.NewReaderSIMAdapter(simProviderStub{}),
			Prepared: identity.PreparedSession{Profile: identity.Profile{IMSI: "001010000000001"}},
		},
		Modem: runtimeStartTestModem{},
	})
	if err != nil {
		t.Fatalf("StartRuntime() error = %v", err)
	}
	if captured.Mode != runtimehost.StartModeReader {
		t.Fatalf("StartRequest.Mode = %q, want reader", captured.Mode)
	}
}

func TestManagerStartRuntimeBroadcastsClaimedState(t *testing.T) {
	manager := NewManager()
	deviceID := "dev-broadcast"
	claim := manager.BeginStart(deviceID)
	notifications, unsubscribe := manager.SubscribeState(deviceID)
	defer unsubscribe()
	manager.SetRuntimeStartForTest(func(context.Context, runtimehost.StartRequest) (*runtimehost.Instance, error) {
		return &runtimehost.Instance{}, nil
	})
	_, err := manager.StartRuntime(context.Background(), RuntimeStartRequest{
		DeviceID: deviceID,
		Epoch:    claim.Epoch,
		Prepared: PreparedStart{
			SIM:      runtimehost.NewReaderSIMAdapter(simProviderStub{}),
			Prepared: identity.PreparedSession{Profile: identity.Profile{IMSI: "001010000000001"}},
		},
		Modem: runtimeStartTestModem{},
	})
	if err != nil {
		t.Fatalf("StartRuntime: %v", err)
	}
	select {
	case <-notifications:
	case <-time.After(time.Second):
		t.Fatal("claimed runtime state was not broadcast")
	}
}

func TestManagerStartRuntimePublishesStateBeforeStarterReturns(t *testing.T) {
	manager := NewManager()
	deviceID := "dev-starting-state"
	claim := manager.BeginStart(deviceID)
	notifications, unsubscribe := manager.SubscribeState(deviceID)
	defer unsubscribe()

	eventSent := make(chan struct{})
	release := make(chan struct{})
	wantInst := &runtimehost.Instance{}
	manager.SetRuntimeStartForTest(func(ctx context.Context, req runtimehost.StartRequest) (*runtimehost.Instance, error) {
		req.Observer.OnRuntimeHostEvent(ctx, runtimehost.Event{
			Kind: "prepared", DeviceID: deviceID, Session: wantInst,
			State: runtimehost.State{
				DeviceID: deviceID, Phase: runtimehost.PhaseSIMReady,
				SIMReady: true, AccessReady: true, UpdatedAt: time.Now(),
			},
		})
		close(eventSent)
		<-release
		return wantInst, nil
	})

	result := make(chan error, 1)
	go func() {
		_, err := manager.StartRuntime(context.Background(), RuntimeStartRequest{
			DeviceID: deviceID,
			Epoch:    claim.Epoch,
			Prepared: PreparedStart{
				SIM:      runtimehost.NewReaderSIMAdapter(simProviderStub{}),
				Prepared: identity.PreparedSession{Profile: identity.Profile{IMSI: "001010000000001"}},
			},
			Modem: runtimeStartTestModem{},
		})
		result <- err
	}()

	<-eventSent
	select {
	case <-notifications:
	case <-time.After(time.Second):
		t.Fatal("startup runtime state was not broadcast before starter returned")
	}
	state, ok := manager.State(deviceID)
	if !ok || !state.SIMReady || !state.AccessReady {
		t.Fatalf("startup runtime state = %+v, ok=%v", state, ok)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatalf("StartRuntime: %v", err)
	}
}

func TestManagerStartRuntimeStopsStaleStartedInstance(t *testing.T) {
	manager := NewManager()
	deviceID := "dev-stale"
	claim := manager.BeginStart(deviceID)
	manager.SetRuntimeStartForTest(func(ctx context.Context, req runtimehost.StartRequest) (*runtimehost.Instance, error) {
		manager.InvalidateRuntime(deviceID, "test")
		replacement := manager.BeginStart(deviceID)
		manager.RecordStartupStateForEpoch(deviceID, replacement.Epoch, runtimehost.State{
			DeviceID: deviceID, Phase: runtimehost.PhaseSIMReady, UpdatedAt: time.Now(),
		})
		if req.ShouldRun() {
			t.Fatal("StartRequest.ShouldRun() = true after invalidation, want false")
		}
		return &runtimehost.Instance{}, nil
	})

	result, err := manager.StartRuntime(context.Background(), RuntimeStartRequest{
		DeviceID: deviceID,
		TraceID:  "trace-stale",
		Epoch:    claim.Epoch,
		Prepared: PreparedStart{
			Profile:     identity.Profile{IMSI: "001010000000001"},
			SIM:         runtimehost.NewReaderSIMAdapter(simProviderStub{}),
			Prepared:    identity.PreparedSession{Profile: identity.Profile{IMSI: "001010000000001"}},
			NetworkMode: "LTE",
		},
		Modem: runtimeStartTestModem{},
	})
	if err != nil {
		t.Fatalf("StartRuntime() error = %v", err)
	}
	if !result.Stale {
		t.Fatalf("StartRuntime() stale = false, want true")
	}
	if manager.Active(deviceID) {
		t.Fatal("stale started instance should not become active")
	}
	if state, _ := manager.State(deviceID); state.Phase != runtimehost.PhaseSIMReady || !manager.Starting(deviceID) {
		t.Fatalf("stale completion cleared replacement startup: %+v", state)
	}
}

// Restart and recovery both start the runtime through StartRuntime; each start
// must get the manager's retry store rather than a fresh one (hideck#102).
func TestManagerStartRuntimeKeepsSubscriptionRetriesAcrossStarts(t *testing.T) {
	manager := NewManager()
	var stores []*runtimehost.SubscriptionRetries
	manager.SetRuntimeStartForTest(func(_ context.Context, req runtimehost.StartRequest) (*runtimehost.Instance, error) {
		stores = append(stores, req.SubscriptionRetries)
		return &runtimehost.Instance{}, nil
	})
	start := func(deviceID string) *runtimehost.Instance {
		t.Helper()
		claim := manager.BeginStart(deviceID)
		if !claim.Accepted {
			t.Fatalf("BeginStart(%s) = %+v", deviceID, claim)
		}
		result, err := manager.StartRuntime(context.Background(), RuntimeStartRequest{
			DeviceID: deviceID, Epoch: claim.Epoch,
			Prepared: PreparedStart{SIM: runtimehost.NewReaderSIMAdapter(simProviderStub{})},
			Modem:    runtimeStartTestModem{},
		})
		if err != nil {
			t.Fatalf("StartRuntime(%s) error = %v", deviceID, err)
		}
		return result.Instance
	}
	first := start("dev-1")
	manager.RuntimeStore().DeleteInstance("dev-1", first)
	start("dev-1")
	start("dev-2")
	if len(stores) != 3 || stores[0] == nil || stores[1] != stores[0] || stores[2] != stores[0] {
		t.Fatalf("subscription retry stores = %p", stores)
	}
	if NewManager().subscriptionRetries == stores[0] {
		t.Fatal("a new manager inherited the subscription retry store")
	}
}
