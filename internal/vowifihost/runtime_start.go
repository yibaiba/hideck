package vowifihost

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost"
	"github.com/iniwex5/vowifi-go/runtimehost/eventhost"
	"github.com/iniwex5/vowifi-go/runtimehost/messaging"
	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
)

type runtimeStartFunc func(context.Context, runtimehost.StartRequest) (*runtimehost.Instance, error)

func buildVoWiFiSIMAdapter(override runtimehost.SIMAdapter) (runtimehost.SIMAdapter, error) {
	if override == nil {
		return nil, fmt.Errorf("vowifi runtime start requires an injected SIM AKA provider")
	}
	return override, nil
}

type RuntimeStartRequest struct {
	DeviceID      string
	TraceID       string
	Epoch         uint64
	StartedAt     time.Time
	Prepared      PreparedStart
	Modem         runtimehost.Modem
	Dataplane     runtimehost.DataplanePolicy
	VoiceGateway  *voicehost.Gateway
	DeliveryStore messaging.DeliveryStore
	Dispatch      eventhost.Dispatcher
	BeforeStart   func(context.Context, runtimehost.SessionConfig) error
	TunnelFactory runtimehost.TunnelFactory
}

type RuntimeStartResult struct {
	Instance *runtimehost.Instance
	Stale    bool
}

func (m *Manager) SetRuntimeStartForTest(fn runtimeStartFunc) {
	if m == nil {
		return
	}
	m.runtimeStart = fn
}

func (m *Manager) runtimeStarter() runtimeStartFunc {
	if m != nil && m.runtimeStart != nil {
		return m.runtimeStart
	}
	return runtimehost.Start
}

func (m *Manager) runtimeStateObserver(deviceID string, epoch uint64, readiness *runtimeReadinessTracker) runtimehost.Observer {
	return runtimehost.ObserverFunc(func(_ context.Context, ev runtimehost.Event) {
		stateMu := m.stateLock(deviceID)
		stateMu.Lock()
		defer stateMu.Unlock()
		if !m.ShouldRun(deviceID, epoch) {
			return
		}
		inst := ev.Session
		if inst != nil && m.IsCurrentInstance(deviceID, inst) {
			readiness.Observe(ev)
			m.observeWiFiCallingHealth(deviceID, ev.State)
			m.BroadcastState(deviceID)
			if isTerminalRuntimeFailure(ev.State) {
				go m.releaseFailedRuntime(deviceID, inst, ev.State)
			}
			return
		}
		m.RecordStartupState(deviceID, ev.State)
	})
}

func (m *Manager) StartRuntime(ctx context.Context, req RuntimeStartRequest) (RuntimeStartResult, error) {
	if m == nil {
		return RuntimeStartResult{}, fmt.Errorf("vowifi host manager is nil")
	}
	deviceID := strings.TrimSpace(req.DeviceID)
	if deviceID == "" {
		return RuntimeStartResult{}, fmt.Errorf("vowifi runtime start device_id is empty")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil || !m.ShouldRun(deviceID, req.Epoch) {
		return RuntimeStartResult{}, context.Canceled
	}

	prepared := req.Prepared.Prepared
	profile := prepared.Profile
	if strings.TrimSpace(profile.IMSI) == "" {
		profile = req.Prepared.Profile
	}
	networkMode := strings.TrimSpace(req.Prepared.StartupState.NetworkMode)
	if networkMode == "" {
		networkMode = strings.TrimSpace(req.Prepared.NetworkMode)
	}

	simAdapter, err := buildVoWiFiSIMAdapter(req.Prepared.SIM)
	if err != nil {
		return RuntimeStartResult{}, err
	}
	mode := strings.TrimSpace(req.Prepared.Mode)
	if mode == "" {
		mode = runtimehost.StartModeMain
	}
	readiness := newRuntimeReadinessTracker(m, runtimeReadinessConfig{
		DeviceID: deviceID, TraceID: req.TraceID, StartedAt: req.StartedAt,
	})
	inst, err := m.runtimeStarter()(ctx, runtimehost.StartRequest{
		Mode:          mode,
		DeviceID:      deviceID,
		TraceID:       strings.TrimSpace(req.TraceID),
		Profile:       profile,
		Prepared:      &prepared,
		NetworkMode:   networkMode,
		VoiceGateway:  req.VoiceGateway,
		SIM:           simAdapter,
		Access:        runtimehost.NewModemAccessAdapter(req.Modem),
		Dataplane:     req.Dataplane,
		Proxy:         req.Prepared.Proxy,
		DeliveryStore: req.DeliveryStore,
		Dispatch:      req.Dispatch,
		BeforeStart:   req.BeforeStart,
		TunnelFactory: req.TunnelFactory,
		ShouldRun: func() bool {
			return ctx.Err() == nil && m.ShouldRun(deviceID, req.Epoch)
		},
		Observer: m.runtimeStateObserver(deviceID, req.Epoch, readiness),

		SubscriptionRetries: m.subscriptionRetries,
	})
	if err != nil {
		return RuntimeStartResult{}, err
	}

	if ctx.Err() != nil || !m.ClaimStarted(deviceID, req.Epoch, inst) {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = inst.Stop(stopCtx)
		cancel()
		return RuntimeStartResult{Instance: inst, Stale: true}, nil
	}
	stateMu := m.stateLock(deviceID)
	stateMu.Lock()
	defer stateMu.Unlock()
	if !m.ShouldRun(deviceID, req.Epoch) || !m.IsCurrentInstance(deviceID, inst) {
		return RuntimeStartResult{Instance: inst, Stale: true}, nil
	}
	readiness.Observe(runtimehost.Event{
		Session: inst, TraceID: req.TraceID, State: inst.State(),
	})
	m.BroadcastState(deviceID)

	return RuntimeStartResult{Instance: inst}, nil
}
