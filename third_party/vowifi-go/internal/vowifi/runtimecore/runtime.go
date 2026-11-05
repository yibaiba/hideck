package runtimecore

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"

	"github.com/iniwex5/vowifi-go/engine/ipsec"
	"github.com/iniwex5/vowifi-go/engine/swu"
	"github.com/iniwex5/vowifi-go/internal/vowifi/imscore"
	"github.com/iniwex5/vowifi-go/internal/vowifi/profile"
)

const defaultRuntimeTraceID = "runtime"

func (Runtime) Start(
	ctx context.Context,
	req RuntimeStartRequest,
) (RuntimeStartResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req.DeviceID = strings.TrimSpace(req.DeviceID)
	req.TraceID = firstNonEmpty(req.TraceID, defaultRuntimeTraceID)
	if req.DeviceID == "" {
		return RuntimeStartResult{TraceID: req.TraceID}, errors.New("runtimecore: device ID is required")
	}
	if req.registrarPenalties == nil {
		req.registrarPenalties = imscore.NewRegistrarPenaltyStore()
	}
	if req.epdgCandidates == nil {
		req.epdgCandidates = swu.NewEPDGCandidateStore(ipsec.ResolveUDPAddrAllContext)
	}
	if req.fastReauth == nil {
		req.fastReauth = &FastReauthStore{}
	}
	if req.subscriptionRegistrations == nil {
		req.subscriptionRegistrations = imscore.NewPersistentSubscriptionRegistrationStore(
			req.SubscriptionStore,
		)
	}
	if req.Options.Voice != nil && req.voiceBinding == nil {
		req.voiceBinding = &voiceLifecycleBinding{deviceID: req.DeviceID, voice: req.Options.Voice}
	}
	if !req.Reconnect || req.DryRun {
		return (Runtime{}).startOnce(ctx, &req)
	}
	if req.voiceBinding != nil {
		defer req.voiceBinding.Stop()
	}
	result, err := runReconnectLoop(ctx, &req)
	emitTerminalRuntimeError(ctx, &req, err)
	return result, err
}

func runReconnectLoop(
	ctx context.Context,
	req *RuntimeStartRequest,
) (RuntimeStartResult, error) {
	result := RuntimeStartResult{TraceID: req.TraceID}
	err := RunLoop(
		ctx,
		req.ReconnectDelay,
		func() {},
		func(attempt int, delay int64) { emitRetry(ctx, req, attempt, delay) },
		func(runCtx context.Context) error {
			started, err := (Runtime{}).startOnce(runCtx, req)
			if err != nil {
				return applyRedirectOverride(req, err)
			}
			result = started
			if started.Session == nil {
				return nil
			}
			return applyRedirectOverride(req, runUntilInterrupted(runCtx, req, started.Session))
		},
	)
	return result, err
}

func (Runtime) startOnce(
	ctx context.Context,
	req *RuntimeStartRequest,
) (RuntimeStartResult, error) {
	localRequest := *req
	req = &localRequest
	notifications := gateCandidateNotifications(req)
	notifications.commit()
	completed := false
	defer func() {
		if !completed {
			notifications.retire()
		}
	}()
	result := RuntimeStartResult{TraceID: req.TraceID}
	if req.ShouldRun != nil && !req.ShouldRun() {
		return result, context.Canceled
	}
	if req.SIM == nil {
		err := errors.New("runtimecore: SIM adapter is required")
		emitRuntimeError(ctx, req, err)
		return result, err
	}
	if req.DryRun {
		return result, nil
	}
	prepared, err := PrepareSessionStart(ctx, *req)
	if err != nil {
		emitRuntimeError(ctx, req, err)
		return result, err
	}
	if req.Hooks.OnPrepared != nil {
		req.Hooks.OnPrepared(ctx, prepared)
	}
	emitAllRuntimeEvents(ctx, req, RuntimeEvent[*SessionResult]{
		Kind: "prepared", DeviceID: req.DeviceID, TraceID: req.TraceID,
		Identity: prepared.IMSIdentity,
	})
	notifier := newIMSRegisteredNotifier(ctx, req, prepared.IMSIdentity)
	config := sessionConfigFromRequest(ctx, req, prepared, notifier)
	var tunnelReadyEmitted atomic.Bool
	emitTunnelReady := func(session *SessionResult) {
		snapshot := snapshotFromSessionResult(session)
		if !snapshot.Established || !tunnelReadyEmitted.CompareAndSwap(false, true) {
			return
		}
		emitEstablished(ctx, req, RuntimeStartResult{TraceID: req.TraceID, Session: session}, prepared.IMSIdentity, snapshot)
	}
	config.OnTunnelReady = emitTunnelReady
	if req.BeforeSessionStart != nil {
		if err := req.BeforeSessionStart(ctx, config); err != nil {
			emitRuntimeError(ctx, req, err)
			return result, err
		}
	}
	if req.Hooks.OnConnecting != nil {
		req.Hooks.OnConnecting(ctx)
	}
	emitAllRuntimeEvents(ctx, req, RuntimeEvent[*SessionResult]{
		Kind: "connecting", DeviceID: req.DeviceID, TraceID: req.TraceID,
		Identity: prepared.IMSIdentity,
	})
	starter := req.SessionStarter
	if starter == nil {
		starter = defaultSessionStarter
	}
	session, err := starter(ctx, config)
	if err != nil {
		emitRuntimeError(ctx, req, err)
		return result, err
	}
	if session == nil {
		err := errors.New("runtimecore: session starter returned nil session")
		emitRuntimeError(ctx, req, err)
		return result, err
	}
	notifier.SetSession(session)
	session.notifications = notifications
	completed = true
	result.Session = session
	emitTunnelReady(session)
	return result, nil
}

func sessionConfigFromRequest(
	ctx context.Context,
	req *RuntimeStartRequest,
	prepared profile.PreparedSession,
	notifier *imsRegisteredNotifier,
) SessionConfig {
	config := SessionConfig{
		Ctx: ctx, DeviceID: req.DeviceID, TraceID: req.TraceID, Prepared: prepared,
		SIM: req.SIM, DataplaneMode: firstNonEmpty(req.Dataplane.Mode, swu.DataplaneModeUserspace),
		TUNName: req.Dataplane.TUNName,
		Proxy:   req.Proxy, DNSServer: req.DNSServer, DeliveryStore: req.DeliveryStore,
		Dispatch: req.Dispatch, OnIMSRegistered: notifier.OnIMSRegistered,
		OnSMSReady: notifier.OnSMSReady,
		OnSMSReadinessChanged: func(readiness imscore.SMSReadiness) {
			if req.Hooks.OnSMSReadinessChanged != nil {
				req.Hooks.OnSMSReadinessChanged(ctx, readiness)
			}
		},
		OnProgress: req.OnProgress, OmitInitialContact: req.omitInitialContact,
		RegistrarPenalties:        req.registrarPenalties,
		EPDGCandidates:            req.epdgCandidates,
		SubscriptionRegistrations: req.subscriptionRegistrations,
		MWIPolicyStore:            req.MWIPolicyStore,
	}
	req.fastReauth.Apply(&config)
	return config
}

func emitEstablished(
	ctx context.Context,
	req *RuntimeStartRequest,
	result RuntimeStartResult,
	identity profile.IMSIdentityResult,
	snapshot Snapshot,
) {
	event := RuntimeEvent[*SessionResult]{
		Kind: "established", Handle: result.Session, DeviceID: req.DeviceID,
		TraceID: req.TraceID, Identity: identity, Snapshot: snapshot,
	}
	if result.Session != nil {
		event.Service = result.Session.IMSService
	}
	emitAllRuntimeEvents(ctx, req, event)
	if req.Hooks.OnEstablished != nil {
		req.Hooks.OnEstablished(ctx, result)
	}
}
