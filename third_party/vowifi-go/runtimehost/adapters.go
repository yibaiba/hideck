package runtimehost

import (
	"context"
	"errors"
	"time"

	enginesim "github.com/iniwex5/vowifi-go/engine/sim"
	"github.com/iniwex5/vowifi-go/internal/vowifi/access"
	"github.com/iniwex5/vowifi-go/internal/vowifi/imscore"
	"github.com/iniwex5/vowifi-go/internal/vowifi/profile"
	"github.com/iniwex5/vowifi-go/internal/vowifi/smsdelivery"
	"github.com/iniwex5/vowifi-go/internal/vowifi/voice"
	"github.com/iniwex5/vowifi-go/runtimehost/messaging"
)

// serviceAdapter adapts an imscore.Service to the runtimehost.Service surface.
type serviceAdapter struct {
	svc *imscore.Service
}

func attachVoiceAgent(req StartRequest, inst *Instance, lifecycle IMSLifecycle) error {
	if req.VoiceGateway == nil {
		return nil
	}
	adapter, ok := lifecycle.(*imscoreLifecycleAdapter)
	if !ok || adapter.svc == nil {
		return errors.New("runtimehost: voice requires the registered IMS service")
	}
	if err := req.VoiceGateway.AttachDeviceCurrent(req.DeviceID, adapter.svc); err != nil {
		return err
	}
	agent, ok := req.VoiceGateway.GetAgent(req.DeviceID).(*voice.Agent)
	if !ok || agent == nil {
		req.VoiceGateway.DetachDeviceCurrent(req.DeviceID)
		return errors.New("runtimehost: voice gateway did not retain the registered agent")
	}
	adapter.svc.SetVoiceRequestHandler(agent)
	inst.setVoiceDetach(func() error {
		adapter.svc.SetVoiceRequestHandler(nil)
		req.VoiceGateway.DetachDeviceCurrent(req.DeviceID)
		return nil
	})
	return nil
}

// newServiceAdapter wraps an imscore service.
func newServiceAdapter(svc *imscore.Service) *serviceAdapter {
	return &serviceAdapter{svc: svc}
}

// Register runs the real IMS REGISTER flow.
func (a serviceAdapter) Register(ctx context.Context) error {
	if a.svc == nil {
		return errNoService
	}
	return a.svc.Register(ctx)
}

func (a serviceAdapter) RegistrationErrors() <-chan error {
	if a.svc == nil {
		return nil
	}
	return a.svc.RegistrationErrors()
}

func (a serviceAdapter) SMSReadiness() SMSReadiness {
	if a.svc == nil {
		return SMSReadiness{Reason: "IMS service is unavailable"}
	}
	return adaptSMSReadiness(a.svc.SMSReadiness())
}

func (a serviceAdapter) SetOnSMSReadinessChanged(fn func(SMSReadiness)) {
	if a.svc == nil {
		return
	}
	a.svc.SetOnSMSReadinessChanged(func(readiness imscore.SMSReadiness) {
		if fn != nil {
			fn(adaptSMSReadiness(readiness))
		}
	})
}

func adaptSMSReadiness(readiness imscore.SMSReadiness) SMSReadiness {
	return SMSReadiness{
		ReceiverPending: readiness.ReceiverPending,
		Registered:      readiness.Registered, ProfileReady: readiness.ProfileReady,
		TransportReady: readiness.TransportReady, ReceiverReady: readiness.ReceiverReady,
		SMSCPresent: readiness.SMSCPresent, Ready: readiness.Ready,
		MOReady: readiness.MOReady, HealthReady: readiness.HealthReady, Reason: readiness.Reason,
	}
}

func (a serviceAdapter) SetSMSMemoryFull(full bool) {
	if a.svc != nil {
		a.svc.SetSMSMemoryFull(full)
	}
}

func (a serviceAdapter) WakeMWISubscription() bool {
	return a.svc.WakeMWISubscription()
}

// SendSMSWithOptions sends an SMS with options.
func (a serviceAdapter) SendSMSWithOptions(ctx context.Context, to, text string, opts messaging.SendOptions) (messaging.SendOutcome, error) {
	if a.svc == nil {
		return messaging.SendOutcome{}, errNoService
	}
	if opts.SuppressSendTGSuccess {
		ctx = messaging.WithSuppressSendTGSuccess(ctx)
	}
	out, err := a.svc.SendSMSWithOptions(ctx, to, text, imscore.SendOptions{Encoding: opts.Encoding})
	return adaptSMSSendOutcome(out), err
}

// SendSMSWithResult sends an SMS.
func (a serviceAdapter) SendSMSWithResult(ctx context.Context, to, text string) (messaging.SendOutcome, error) {
	if a.svc == nil {
		return messaging.SendOutcome{}, errNoService
	}
	out, err := a.svc.SendSMSWithResult(ctx, to, text)
	return adaptSMSSendOutcome(out), err
}

func adaptSMSSendOutcome(out imscore.SendOutcome) messaging.SendOutcome {
	return messaging.SendOutcome{
		Ref:                 out.MessageID,
		MessageID:           out.MessageID,
		PartsTotal:          out.PartsTotal,
		DeliveryState:       out.DeliveryState,
		SIPCode:             out.SIPCode,
		RecommendCSFallback: out.RecommendCSFallback,
	}
}

// GetSMSDeliveryStatus returns the delivery status of an SMS.
func (a serviceAdapter) GetSMSDeliveryStatus(ref string) (*messaging.DeliveryStatus, error) {
	if a.svc == nil {
		return nil, errNoService
	}
	st, err := a.svc.GetSMSDeliveryStatus(ref)
	if err != nil {
		return nil, deliveryStoreErrorFromInternal(err)
	}
	return deliveryStatusFromInternal(st), nil
}

// SendUSSD sends a USSD request.
func (a serviceAdapter) SendUSSD(ctx context.Context, code string) (*messaging.USSDResult, error) {
	if a.svc == nil {
		return nil, errNoService
	}
	res, err := a.svc.SendUSSD(ctx, code)
	if err != nil {
		return nil, err
	}
	return messagingUSSDResult(res), nil
}

// ContinueUSSD continues a USSD session.
func (a serviceAdapter) ContinueUSSD(ctx context.Context, sessionID, input string) (*messaging.USSDResult, error) {
	if a.svc == nil {
		return nil, errNoService
	}
	res, err := a.svc.ContinueUSSD(ctx, sessionID, input)
	if err != nil {
		return nil, err
	}
	return messagingUSSDResult(res), nil
}

func messagingUSSDResult(result *imscore.USSDResult) *messaging.USSDResult {
	if result == nil {
		return nil
	}
	return &messaging.USSDResult{
		SessionID: result.SessionID, Status: result.Status, Text: result.Text,
		RawXML: result.RawXML, RawText: result.RawXML, DCS: result.DCS, Message: result.Text,
	}
}

// CancelUSSD cancels a USSD session.
func (a serviceAdapter) CancelUSSD(ctx context.Context, sessionID string) error {
	if a.svc == nil {
		return errNoService
	}
	return a.svc.CancelUSSD(ctx, sessionID)
}

// Status returns the original map projection.
func (a serviceAdapter) Status() map[string]interface{} {
	if a.svc == nil {
		return nil
	}
	status := a.messagingStatusSnapshot()
	return map[string]interface{}{
		"enabled": status.Enabled, "device_id": status.DeviceID,
		"registered": status.IsRegistered(), "reg_status": status.RegStatus,
		"registrar": status.Registrar, "local_addr": status.LocalAddr,
	}
}

func (a serviceAdapter) messagingStatusSnapshot() messaging.ServiceStatus {
	if a.svc == nil {
		return messaging.ServiceStatus{}
	}
	status := a.svc.StatusSnapshot()
	return messaging.ServiceStatus{
		Enabled: status.Enabled, DeviceID: status.DeviceID, Registered: status.Registered,
		RegStatus: status.RegStatus, Registrar: status.Registrar, LocalAddr: status.LocalAddr,
		AssociatedMSISDN: status.AssociatedMSISDN, LastSIPCode: status.LastSIPCode,
		LastSIPText: status.LastSIPText, PingFailCount: status.PingFailCount,
		LastSMSAt: status.LastSMSSendAt, LastSMSError: status.LastSMSSendErr,
		State: status.State, RegState: status.RegState,
	}
}

func boolStatus(value bool) int {
	if value {
		return 1
	}
	return 0
}

// StatusSnapshot returns the original structured status snapshot.
func (a serviceAdapter) StatusSnapshot() messaging.ServiceStatus {
	return a.messagingStatusSnapshot()
}

// Stop shuts the service down.
func (a serviceAdapter) Stop(ctx context.Context) error {
	if a.svc == nil {
		return nil
	}
	return a.svc.Stop(ctx)
}

// TriggerRegisterImmediate triggers an immediate re-registration.
func (a serviceAdapter) TriggerRegisterImmediate(reason string) {
	if a.svc != nil {
		a.svc.TriggerRegisterImmediate(reason)
	}
}

func (a serviceAdapter) GetSMSDeliveryStatusContext(
	ctx context.Context,
	ref string,
) (*messaging.DeliveryStatus, error) {
	if a.svc == nil {
		return nil, errNoService
	}
	status, err := a.svc.GetSMSDeliveryStatusContext(ctx, ref)
	if err != nil {
		return nil, deliveryStoreErrorFromInternal(err)
	}
	return deliveryStatusFromInternal(status), nil
}

func (a serviceAdapter) StatusCurrent() Status {
	status := a.messagingStatusSnapshot()
	sms := SMSReadiness{}
	if a.svc != nil {
		sms = adaptSMSReadiness(a.svc.SMSReadiness())
	}
	return Status{State: State{
		Phase: "ready", DeviceID: status.DeviceID,
		IMSReady: status.IsRegistered(), SMSReady: sms.Ready, SMSMOReady: sms.MOReady, SMSHealthReady: sms.HealthReady,
		SMSRXPending: sms.ReceiverPending,
		RegStatus:    boolStatus(status.IsRegistered()), RegStatusText: status.RegStatus,
		SessionState: "established", IMSState: status.RegState, SMSReadyReason: sms.Reason,
	}}
}

func (a serviceAdapter) StopCurrent() {
	_ = a.Stop(context.Background())
}

func (a serviceAdapter) TriggerRegisterImmediateCurrent() error {
	if a.svc == nil {
		return errNoService
	}
	return a.svc.TriggerRegisterImmediateCurrent()
}

// deliveryStoreAdapter adapts a messaging.DeliveryStore to the imscore
// DeliveryStore surface.
type deliveryStoreAdapter struct {
	store messaging.DeliveryStore
}

type sipResultDeliveryStoreAdapter struct {
	*deliveryStoreAdapter
	store messaging.SIPResultStore
}

type inboundFragmentCapability struct {
	store messaging.InboundFragmentStore
}

type inboundFragmentLifecycleCapability struct {
	inboundFragmentCapability
	lifecycle messaging.InboundFragmentLifecycleStore
}

type fragmentDeliveryStoreAdapter struct {
	*deliveryStoreAdapter
	inboundFragmentCapability
}

type lifecycleFragmentDeliveryStoreAdapter struct {
	*deliveryStoreAdapter
	inboundFragmentLifecycleCapability
}

type completeDeliveryStoreAdapter struct {
	*sipResultDeliveryStoreAdapter
	inboundFragmentCapability
}

type completeLifecycleDeliveryStoreAdapter struct {
	*sipResultDeliveryStoreAdapter
	inboundFragmentLifecycleCapability
}

// newDeliveryStoreAdapter wraps a delivery store.
func newDeliveryStoreAdapter(store messaging.DeliveryStore) imscore.DeliveryStore {
	base := &deliveryStoreAdapter{store: store}
	sipResults, hasSIPResults := store.(messaging.SIPResultStore)
	fragments, hasFragments := store.(messaging.InboundFragmentStore)
	lifecycle, hasLifecycle := store.(messaging.InboundFragmentLifecycleStore)
	switch {
	case hasSIPResults && hasFragments && hasLifecycle:
		sipAdapter := &sipResultDeliveryStoreAdapter{deliveryStoreAdapter: base, store: sipResults}
		return &completeLifecycleDeliveryStoreAdapter{
			sipResultDeliveryStoreAdapter: sipAdapter,
			inboundFragmentLifecycleCapability: inboundFragmentLifecycleCapability{
				inboundFragmentCapability: inboundFragmentCapability{store: fragments},
				lifecycle:                 lifecycle,
			},
		}
	case hasSIPResults && hasFragments:
		sipAdapter := &sipResultDeliveryStoreAdapter{deliveryStoreAdapter: base, store: sipResults}
		return &completeDeliveryStoreAdapter{
			sipResultDeliveryStoreAdapter: sipAdapter,
			inboundFragmentCapability:     inboundFragmentCapability{store: fragments},
		}
	case hasSIPResults:
		return &sipResultDeliveryStoreAdapter{deliveryStoreAdapter: base, store: sipResults}
	case hasFragments && hasLifecycle:
		return &lifecycleFragmentDeliveryStoreAdapter{
			deliveryStoreAdapter: base,
			inboundFragmentLifecycleCapability: inboundFragmentLifecycleCapability{
				inboundFragmentCapability: inboundFragmentCapability{store: fragments},
				lifecycle:                 lifecycle,
			},
		}
	case hasFragments:
		return &fragmentDeliveryStoreAdapter{
			deliveryStoreAdapter:      base,
			inboundFragmentCapability: inboundFragmentCapability{store: fragments},
		}
	default:
		return base
	}
}

func (a inboundFragmentLifecycleCapability) MarkInboundFragmentsDegraded(
	scope messaging.InboundFragmentScope,
	at time.Time,
) error {
	return a.lifecycle.MarkInboundFragmentsDegraded(scope, at)
}

func (a inboundFragmentCapability) LoadInboundFragments(
	owner messaging.InboundFragmentOwner,
) ([]messaging.StoredInboundFragment, error) {
	return a.store.LoadInboundFragments(owner)
}

func (a inboundFragmentCapability) SaveInboundFragment(
	scope messaging.InboundFragmentScope,
	fragment messaging.InboundFragment,
) (messaging.InboundFragmentSaveResult, error) {
	return a.store.SaveInboundFragment(scope, fragment)
}

func (a inboundFragmentCapability) DeleteInboundFragments(scope messaging.InboundFragmentScope) error {
	return a.store.DeleteInboundFragments(scope)
}

func (a inboundFragmentCapability) MarkInboundFragmentAcked(
	scope messaging.InboundFragmentScope,
	sequence int,
	at time.Time,
) error {
	return a.store.MarkInboundFragmentAcked(scope, sequence, at)
}

func (a *sipResultDeliveryStoreAdapter) MarkSMSDeliveryPartSIPResult(
	messageID string,
	partNo, sipCode int,
	state, errText string,
	at time.Time,
) error {
	if a == nil || a.store == nil {
		return errors.New("runtimehost: no SIP result store")
	}
	return a.store.MarkSMSDeliveryPartSIPResult(messageID, partNo, sipCode, state, errText, at)
}

// CreateSMSDelivery creates a delivery record.
func (a *deliveryStoreAdapter) CreateSMSDelivery(
	messageID, imsi, deviceID, peer, content string,
	partsTotal int,
	at time.Time,
) error {
	if a == nil || a.store == nil {
		return errors.New("runtimehost: no delivery store")
	}
	return deliveryStoreErrorToInternal(
		a.store.CreateSMSDelivery(messageID, imsi, deviceID, peer, content, partsTotal, at),
	)
}

func (a *deliveryStoreAdapter) CreateSMSDeliveryForIdentity(record smsdelivery.DeliveryRecord) error {
	if a == nil || a.store == nil {
		return errors.New("runtimehost: no delivery store")
	}
	if store, ok := a.store.(messaging.IdentityDeliveryStore); ok {
		return deliveryStoreErrorToInternal(store.CreateSMSDeliveryForIdentity(record))
	}
	return a.CreateSMSDelivery(
		record.MessageID, record.IMSI, record.DeviceID,
		record.Peer, record.Content, record.PartsTotal, record.At,
	)
}

// UpsertSMSDeliveryPart upserts a delivery part.
func (a *deliveryStoreAdapter) UpsertSMSDeliveryPart(messageID string, partNo int, callID string, rpMR int, state string, sentAt time.Time) error {
	if a == nil || a.store == nil {
		return errors.New("runtimehost: no delivery store")
	}
	return deliveryStoreErrorToInternal(
		a.store.UpsertSMSDeliveryPart(messageID, partNo, callID, rpMR, state, sentAt),
	)
}

// MarkSMSDeliveryPartReport records a delivery report.
func (a *deliveryStoreAdapter) MarkSMSDeliveryPartReport(inReplyTo, callID, deviceID string, rpMR int, state string, sipCode int, rpCause int, errText string, at time.Time) (imscore.DeliveryPartMatch, error) {
	if a == nil || a.store == nil {
		return imscore.DeliveryPartMatch{}, errors.New("runtimehost: no delivery store")
	}
	m, err := a.store.MarkSMSDeliveryPartReport(inReplyTo, callID, deviceID, rpMR, state, sipCode, rpCause, errText, at)
	return imscore.DeliveryPartMatch{
		MessageID: m.MessageID,
		PartNo:    m.PartNo,
		State:     m.State,
		Matched:   m.Matched || m.MessageID != "",
	}, deliveryStoreErrorToInternal(err)
}

// RecomputeSMSDelivery recomputes the delivery state.
func (a *deliveryStoreAdapter) RecomputeSMSDelivery(messageID string, at time.Time) error {
	if a == nil || a.store == nil {
		return errors.New("runtimehost: no delivery store")
	}
	return deliveryStoreErrorToInternal(a.store.RecomputeSMSDelivery(messageID, at))
}

// UpdateSMSDeliveryState updates the delivery state.
func (a *deliveryStoreAdapter) UpdateSMSDeliveryState(messageID, state, lastError string, acks int, at time.Time) error {
	if a == nil || a.store == nil {
		return errors.New("runtimehost: no delivery store")
	}
	return deliveryStoreErrorToInternal(
		a.store.UpdateSMSDeliveryState(messageID, state, lastError, acks, at),
	)
}

// GetSMSDeliveryStatus returns the delivery status.
func (a *deliveryStoreAdapter) GetSMSDeliveryStatus(messageID string) (*imscore.DeliveryStatus, error) {
	if a == nil || a.store == nil {
		return nil, errors.New("runtimehost: no delivery store")
	}
	st, err := a.store.GetSMSDeliveryStatus(messageID)
	if err != nil {
		return nil, deliveryStoreErrorToInternal(err)
	}
	return deliveryStatusToInternal(st), nil
}

// deliveryStatusFromInternal converts an imscore delivery status to messaging.
func deliveryStatusFromInternal(st *imscore.DeliveryStatus) *messaging.DeliveryStatus {
	if st == nil {
		return nil
	}
	out := &messaging.DeliveryStatus{
		MessageID:  st.MessageID,
		IMSI:       st.IMSI,
		ICCID:      st.ICCID,
		DeviceID:   st.DeviceID,
		Peer:       st.Peer,
		Content:    st.Content,
		PartsTotal: st.PartsTotal,
		Acks:       st.Acks,
		State:      st.State,
		LastError:  st.LastError,
		CreatedAt:  st.CreatedAt,
		UpdatedAt:  st.UpdatedAt,
	}
	for _, p := range st.Parts {
		out.Parts = append(out.Parts, messaging.DeliveryPartStatus{
			PartNo: p.PartNo, CallID: p.CallID, InReplyTo: p.InReplyTo, RPMR: p.RPMR,
			State: p.State, SIPCode: p.SIPCode, RPCause: p.RPCause,
			RPCauseText: p.RPCauseText, ErrorText: p.ErrorText,
			SentAt: p.SentAt, ReportAt: p.ReportAt, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
		})
	}
	return out
}

// deliveryStatusToInternal converts a messaging delivery status to imscore.
func deliveryStatusToInternal(st *messaging.DeliveryStatus) *imscore.DeliveryStatus {
	if st == nil {
		return nil
	}
	out := &imscore.DeliveryStatus{
		MessageID:  st.MessageID,
		IMSI:       st.IMSI,
		ICCID:      st.ICCID,
		DeviceID:   st.DeviceID,
		Peer:       st.Peer,
		Content:    st.Content,
		PartsTotal: st.PartsTotal,
		Acks:       st.Acks,
		State:      st.State,
		LastError:  st.LastError,
		CreatedAt:  st.CreatedAt,
		UpdatedAt:  st.UpdatedAt,
	}
	for _, p := range st.Parts {
		out.Parts = append(out.Parts, imscore.DeliveryPartStatus{
			PartNo: p.PartNo, CallID: p.CallID, InReplyTo: p.InReplyTo, RPMR: p.RPMR,
			State: p.State, SIPCode: p.SIPCode, RPCause: p.RPCause,
			RPCauseText: p.RPCauseText, ErrorText: p.ErrorText,
			SentAt: p.SentAt, ReportAt: p.ReportAt, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
		})
	}
	return out
}

// OnRuntimeHostEvent implements ObserverFunc as a method.
func (f ObserverFunc) OnRuntimeHostEvent(ctx context.Context, ev Event) {
	if f != nil {
		f(ctx, ev)
	}
}

// simAdapter adapts a SIM provider.
type simAdapter struct {
	inner access.SIMAdapter
}

func (a simAdapter) runtimeSIMAdapter() access.SIMAdapter {
	return a.inner
}

// AKAProvider preserves source compatibility for concrete adapter users.
func (a simAdapter) AKAProvider() enginesim.AKAProvider {
	if a.inner == nil {
		return nil
	}
	return a.inner.EPDGSIMProvider(profile.AuthPlan{})
}

// SIMProvider computes AKA through the injected SIM implementation.
type SIMProvider = enginesim.AKAProvider

// apiErrorToInternal converts an API error to an internal error.
func apiErrorToInternal(err error) error {
	return err
}

// defaultMainReconnectDelay returns the default reconnect delay for main mode.
func defaultMainReconnectDelay(attempt int) int64 {
	if attempt < 1 {
		return int64(5 * time.Second)
	}
	return int64(30 * time.Second)
}

// defaultReaderReconnectDelay returns the default reconnect delay for reader mode.
func defaultReaderReconnectDelay(attempt int) int64 {
	switch {
	case attempt < 1:
		return int64(30 * time.Second)
	case attempt == 1:
		return int64(60 * time.Second)
	default:
		return int64(120 * time.Second)
	}
}

// deliveryStoreErrorFromInternal converts an internal delivery error.
func deliveryStoreErrorFromInternal(err error) error {
	if errors.Is(err, smsdelivery.ErrDeliveryNotFound) && !errors.Is(err, messaging.ErrDeliveryNotFound) {
		return errors.Join(messaging.ErrDeliveryNotFound, err)
	}
	return err
}

// deliveryStoreErrorToInternal converts an external delivery error.
func deliveryStoreErrorToInternal(err error) error {
	if errors.Is(err, messaging.ErrDeliveryNotFound) && !errors.Is(err, smsdelivery.ErrDeliveryNotFound) {
		return errors.Join(smsdelivery.ErrDeliveryNotFound, err)
	}
	return err
}
