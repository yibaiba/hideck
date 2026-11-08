package runtimehost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/iniwex5/vowifi-go/internal/vowifi/common"
	"github.com/iniwex5/vowifi-go/internal/vowifi/logging"
	"github.com/iniwex5/vowifi-go/internal/vowifi/runtimecore"
	"github.com/iniwex5/vowifi-go/runtimehost/messaging"
)

// State returns a copy of the current runtime state.
func (i *Instance) State() State {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.state
}

// Service returns the current IMS service (nil until set).
func (i *Instance) Service() Service {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.service
}

// AddObserver registers an observer that receives runtime events.
func (i *Instance) AddObserver(obs Observer) func() {
	if i == nil || obs == nil {
		return func() {}
	}
	i.mu.Lock()
	i.observers = append(i.observers, obs)
	index := len(i.observers) - 1
	i.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			i.mu.Lock()
			if index < len(i.observers) {
				i.observers[index] = nil
			}
			i.mu.Unlock()
		})
	}
}

// SetNotifier installs the session notifier.
func (i *Instance) SetNotifier(n Notifier) {
	i.mu.Lock()
	i.onNotify = n
	i.mu.Unlock()
}

// SetSMSNotifier installs the SMS notifier.
func (i *Instance) SetSMSNotifier(n SMSNotifier) {
	i.mu.Lock()
	i.onSMS = n
	i.mu.Unlock()
}

// Stop shuts the runtime host down.
func (i *Instance) Stop(ctx context.Context) error {
	if i == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	i.mu.Lock()
	if i.stopped {
		i.mu.Unlock()
		return nil
	}
	i.stopped = true
	svc := i.service
	tunnel := i.tunnel
	cancel := i.cancel
	voiceDetach := i.voiceDetach
	i.service = nil
	i.session = nil
	i.tunnel = nil
	i.cancel = nil
	i.voiceDetach = nil
	i.mu.Unlock()
	var stopErr error
	if voiceDetach != nil {
		stopErr = voiceDetach()
	}
	if svc != nil {
		stopErr = errors.Join(stopErr, svc.Stop(ctx))
	}
	if cancel != nil {
		cancel()
	}
	if tunnel != nil {
		tunnel.Shutdown()
	}
	i.updateStateWithEvent(ctx, "stopped", func(state *State) {
		state.Phase = "stopped"
		state.LastEvent = "stopped"
		state.SessionState = "stopped"
		state.DataPlaneUp = false
		state.TunnelReady = false
		state.IMSReady = false
		state.SMSReady = false
		state.SMSRXPending = false
		state.SMSMOReady = false
		state.SMSHealthReady = false
		state.LastReason = "stopped"
	})
	if tunnel == nil {
		return stopErr
	}
	return errors.Join(stopErr, tunnel.WaitDoneContext(ctx))
}

// StopShared stops the host without tearing down shared resources.
func (i *Instance) StopShared(ctx context.Context) error {
	return i.Stop(ctx)
}

// RuntimeState returns the current runtime state.
func (i *Instance) RuntimeState() State {
	return i.State()
}

// Status returns a human-readable status string.
func (i *Instance) Status() string {
	st := i.State()
	if st.LastError != "" {
		return "VoWiFi: " + st.LastError
	}
	phase := firstNonEmptyString(st.Phase, st.SessionState)
	if phase == "" {
		return "VoWiFi: STOPPED"
	}
	return "VoWiFi: " + phase
}

// Obs returns an observation map of the runtime state.
func (i *Instance) Obs() map[string]interface{} {
	st := i.State()
	return map[string]interface{}{
		"phase":            st.Phase,
		"device_id":        st.DeviceID,
		"tunnel_ready":     st.TunnelReady,
		"ims_ready":        st.IMSReady,
		"sms_ready":        st.SMSReady,
		"sms_mo_ready":     st.SMSMOReady,
		"sms_health_ready": st.SMSHealthReady,
		"sms_rx_pending":   st.SMSRXPending,
		"session_state":    st.SessionState,
		"ims_state":        st.IMSState,
		"epdg":             st.EPDGAddress,
		"nat":              st.NATDetected,
	}
}

// setState updates the runtime state.
func (i *Instance) setState(s State) {
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = time.Now()
	}
	i.mu.Lock()
	i.state = s
	i.mu.Unlock()
}

func (i *Instance) mutateState(update func(*State)) State {
	i.mu.Lock()
	update(&i.state)
	i.state.UpdatedAt = time.Now()
	state := i.state
	i.mu.Unlock()
	return state
}

func (i *Instance) updateState(update func(*State)) {
	i.updateStateWithEvent(context.Background(), "state", update)
}

func (i *Instance) updateStateWithEvent(
	ctx context.Context,
	kind string,
	update func(*State),
) {
	state := i.mutateState(update)
	detail := firstNonEmptyString(state.Phase, state.SessionState)
	i.publish(ctx, Event{
		Kind: kind, DeviceID: state.DeviceID, Reason: state.LastReason, State: state,
		Type: kind, Detail: detail, Session: i,
	})
}

func (i *Instance) attachTunnel(tunnel Tunnel, cancel context.CancelFunc) {
	i.mu.Lock()
	i.tunnel = tunnel
	i.cancel = cancel
	i.mu.Unlock()
}

func (i *Instance) updateTunnelState(sessionState string) {
	i.updateState(func(state *State) {
		state.Phase = sessionState
		state.LastEvent = sessionState
		state.SessionState = sessionState
		state.TunnelReady = sessionState == "established"
		state.DataPlaneUp = state.TunnelReady
		if sessionState == "error" || sessionState == "shutdown" {
			state.IMSState = "failed"
			state.IMSReady = false
			state.SMSReady = false
			state.SMSRXPending = false
			state.SMSMOReady = false
			state.SMSHealthReady = false
			state.RegStatus = 0
			state.RegStatusText = "failed"
		}
	})
}

func (i *Instance) markTunnelReadyForIMS() {
	i.updateState(func(state *State) {
		state.Phase = "ipsec_up"
		state.LastEvent = "ipsec_up"
		state.SessionState = "registering"
		state.IMSState = "registering"
		state.TunnelReady = true
		state.DataPlaneUp = true
		state.LastReason = "SWu tunnel established; registering IMS"
	})
}

func (i *Instance) markIMSRegistered() {
	i.updateState(func(state *State) {
		state.Phase = "ims_ready"
		state.LastEvent = "ims_registered"
		state.SessionState = "established"
		state.IMSState = "registered"
		state.IMSReady = true
		state.SMSReady = false
		state.SMSRXPending = true
		state.SMSMOReady = false
		state.SMSReadyReason = "IMS SMS readiness has not been reported"
		state.RegStatus = 1
		state.RegStatusText = "registered"
		state.LastReason = "IMS registered"
	})
}

func (i *Instance) updateSMSReadiness(readiness SMSReadiness) {
	i.updateState(func(state *State) {
		applySMSReadiness(state, readiness)
	})
}

func applySMSReadiness(state *State, readiness SMSReadiness) {
	if runtimeTakenDown(state.Phase) {
		// The IMS service of a runtime already taken down can still publish;
		// its snapshot must not raise SMS or health readiness again.
		logging.Info("runtime SMS readiness ignored after the runtime went down",
			"device", state.DeviceID, "phase", state.Phase, "ready", readiness.Ready,
			"health_ready", readiness.HealthReady, "reason", readiness.Reason)
		return
	}
	state.SMSRXPending = readiness.Registered && readiness.ReceiverPending && !readiness.Ready && !readiness.HealthReady
	state.SMSReady = state.IMSReady && readiness.Ready
	state.SMSMOReady = state.IMSReady && readiness.MOReady
	state.SMSHealthReady = readiness.Registered && (readiness.Ready || readiness.HealthReady)
	state.SMSReadyReason = readiness.Reason
	if state.SMSReady {
		state.Phase = "sms_ready"
		state.LastEvent = "sms_ready"
		clearRecoveredFailure(state)
		return
	}
	if state.IMSReady {
		state.Phase = "ims_ready"
		state.LastEvent = "sms_unavailable"
		if state.SMSRXPending {
			state.LastEvent = "sms_pending"
		}
	}
}

// runtimeTakenDown reports a phase entered when the runtime was interrupted,
// retried, restarted, failed or stopped; readiness returns with the next
// ims_registered.
func runtimeTakenDown(phase string) bool {
	switch phase {
	case "interrupted", "retrying", "restarting", "error", "stopped":
		return true
	}
	return false
}

func (i *Instance) setStartFailure(err error) {
	i.updateState(func(state *State) {
		state.Phase = "error"
		state.LastEvent = "terminal_error"
		state.SessionState = "error"
		state.Error = err.Error()
		state.LastError = err.Error()
		state.LastErrorClass = "network"
		state.LastReason = "SWu tunnel establishment failed"
		state.TunnelReady = false
		state.DataPlaneUp = false
	})
}

func (i *Instance) setIMSFailure(err error) {
	i.updateState(func(state *State) {
		state.Phase = "error"
		state.LastEvent = "terminal_error"
		state.SessionState = "error"
		state.IMSState = "failed"
		state.Error = err.Error()
		state.LastError = err.Error()
		state.LastErrorClass = "ims"
		state.LastReason = "IMS registration failed"
		state.TunnelReady = false
		state.DataPlaneUp = false
		state.IMSReady = false
		state.SMSReady = false
		state.SMSRXPending = false
		state.SMSMOReady = false
		state.SMSHealthReady = false
		state.RegStatus = 0
		state.RegStatusText = "failed"
	})
}

func (i *Instance) setIMSRefreshFailure(err error) {
	i.updateState(func(state *State) {
		state.Phase = "error"
		state.LastEvent = "terminal_error"
		state.SessionState = "error"
		state.IMSState = "failed"
		state.Error = err.Error()
		state.LastError = err.Error()
		state.LastErrorClass = "ims"
		state.LastReason = "IMS registration refresh failed"
		state.IMSReady = false
		state.SMSReady = false
		state.SMSRXPending = false
		state.SMSMOReady = false
		state.SMSHealthReady = false
		state.RegStatus = 0
		state.RegStatusText = "failed"
	})
}

func (i *Instance) setTunnelControlFailure(err error) {
	i.updateState(func(state *State) {
		state.Phase = "error"
		state.LastEvent = "terminal_error"
		state.SessionState = "error"
		state.IMSState = "failed"
		state.Error = err.Error()
		state.LastError = err.Error()
		state.LastErrorClass = "network"
		state.LastReason = "SWu tunnel control failed"
		state.TunnelReady = false
		state.DataPlaneUp = false
		state.IMSReady = false
		state.SMSReady = false
		state.SMSRXPending = false
		state.SMSMOReady = false
		state.SMSHealthReady = false
		state.RegStatus = 0
		state.RegStatusText = "failed"
	})
}

func (i *Instance) setTunnelReauthenticationRequired(err error) {
	i.updateState(func(state *State) {
		state.Phase = "restarting"
		state.LastEvent = "interrupted"
		state.SessionState = "error"
		state.IMSState = "restarting"
		state.Error = err.Error()
		state.LastError = err.Error()
		state.LastErrorClass = ErrorClassReauthentication
		state.LastReason = "IKE reauthentication requires fresh runtime"
		state.TunnelReady = false
		state.DataPlaneUp = false
		state.IMSReady = false
		state.SMSReady = false
		state.SMSRXPending = false
		state.SMSMOReady = false
		state.SMSHealthReady = false
		state.RegStatus = 0
		state.RegStatusText = "restarting"
	})
}

// setService installs the IMS service.
func (i *Instance) setService(s messaging.Service) {
	i.mu.Lock()
	i.service = s
	i.mu.Unlock()
}

func (i *Instance) setVoiceDetach(detach func() error) {
	i.mu.Lock()
	i.voiceDetach = detach
	i.mu.Unlock()
}

// setSession wires a new session into the host.
func (i *Instance) setSession(session *runtimecore.SessionResult) {
	i.mu.Lock()
	i.session = session
	if session != nil && i.startedAt.IsZero() {
		i.startedAt = time.Now()
	}
	i.mu.Unlock()
}

// publish delivers an event to a stable observer snapshot.
func (i *Instance) publish(ctx context.Context, ev Event) {
	if i == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	i.mu.RLock()
	obs := append([]Observer(nil), i.observers...)
	i.mu.RUnlock()
	for _, o := range obs {
		if o != nil {
			o.OnRuntimeHostEvent(ctx, ev)
		}
	}
}

// --- SMS/USSD service delegation ---

// SetSMSMemoryFull tells the IMS SMS receiver whether the host can store MT SMS.
func (i *Instance) SetSMSMemoryFull(full bool) {
	if i == nil {
		return
	}
	svc := i.Service()
	if setter, ok := svc.(interface{ SetSMSMemoryFull(bool) }); ok {
		setter.SetSMSMemoryFull(full)
	}
}

// WakeMWISubscription asks the current IMS service to re-check its MWI
// subscription after the host reset the card's local MWI backoff. It reports
// whether an IMS service took the request; without one the next start reads
// the reset state.
func (i *Instance) WakeMWISubscription() bool {
	if i == nil {
		return false
	}
	waker, ok := i.Service().(interface{ WakeMWISubscription() bool })
	return ok && waker.WakeMWISubscription()
}

// SendSMSWithResult sends an SMS and returns the delivery outcome.
func (i *Instance) SendSMSWithResult(ctx context.Context, to, text string) (messaging.SendOutcome, error) {
	svc := i.Service()
	if svc == nil {
		return messaging.SendOutcome{}, fmt.Errorf("%w: %w", messaging.ErrSMSNotReady, errNoService)
	}
	return svc.SendSMSWithResult(ctx, to, text)
}

// SendSMSWithOptions sends an SMS with delivery options.
func (i *Instance) SendSMSWithOptions(ctx context.Context, to, text string, opts messaging.SendOptions) (messaging.SendOutcome, error) {
	svc := i.Service()
	if svc == nil {
		return messaging.SendOutcome{}, fmt.Errorf("%w: %w", messaging.ErrSMSNotReady, errNoService)
	}
	return svc.SendSMSWithOptions(ctx, to, text, opts)
}

// GetSMSDeliveryStatus returns the delivery status of a previously sent SMS.
func (i *Instance) GetSMSDeliveryStatus(ref string) (*messaging.DeliveryStatus, error) {
	svc := i.Service()
	if svc == nil {
		return nil, errNoService
	}
	return svc.GetSMSDeliveryStatus(ref)
}

// GetSMSDeliveryStatusContext retains the displaced context-aware query API.
func (i *Instance) GetSMSDeliveryStatusContext(
	ctx context.Context,
	ref string,
) (*messaging.DeliveryStatus, error) {
	svc := i.Service()
	if svc == nil {
		return nil, errNoService
	}
	if contextual, ok := svc.(interface {
		GetSMSDeliveryStatusContext(context.Context, string) (*messaging.DeliveryStatus, error)
	}); ok {
		return contextual.GetSMSDeliveryStatusContext(ctx, ref)
	}
	return svc.GetSMSDeliveryStatus(ref)
}

// SendUSSD sends a USSD request.
func (i *Instance) SendUSSD(ctx context.Context, code string) (*messaging.USSDResult, error) {
	svc := i.Service()
	if svc == nil {
		return nil, errNoService
	}
	return svc.SendUSSD(ctx, code)
}

// ContinueUSSD continues a USSD session.
func (i *Instance) ContinueUSSD(ctx context.Context, sessionID, input string) (*messaging.USSDResult, error) {
	svc := i.Service()
	if svc == nil {
		return nil, errNoService
	}
	return svc.ContinueUSSD(ctx, sessionID, input)
}

// CancelUSSD cancels a USSD session.
func (i *Instance) CancelUSSD(ctx context.Context, sessionID string) error {
	svc := i.Service()
	if svc == nil {
		return errNoService
	}
	return svc.CancelUSSD(ctx, sessionID)
}

// TriggerMOBIKE forces a MOBIKE update on the session after an address change.
func (i *Instance) TriggerMOBIKE(oldIP, newIP string) error {
	oldAddress := net.ParseIP(oldIP)
	newAddress := net.ParseIP(newIP)
	if oldAddress == nil || newAddress == nil {
		return errors.New("runtimehost: MOBIKE requires valid old and new IP addresses")
	}
	i.mu.RLock()
	tunnel := i.tunnel
	session := i.session
	i.mu.RUnlock()
	if tunnel == nil && (session == nil || session.Session == nil) {
		return errors.New("runtimehost: no SWu tunnel installed")
	}
	var err error
	if tunnel != nil {
		err = tunnel.UpdateAddresses(oldAddress, newAddress)
	} else {
		err = session.Session.UpdateAddresses(oldAddress, newAddress)
	}
	if err != nil {
		wrapped := fmt.Errorf("runtimehost: MOBIKE address update failed: %w", err)
		i.setTunnelControlFailure(wrapped)
		return wrapped
	}
	return nil
}

// newTraceID returns a random hex trace id.
func newTraceID() string {
	return common.NewTraceID()
}
