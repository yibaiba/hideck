package runtimehost

import (
	"context"

	"github.com/iniwex5/vowifi-go/internal/vowifi/imscore"
	"github.com/iniwex5/vowifi-go/runtimehost/messaging"
)

// lifecycleServiceAdapter exposes an injected current lifecycle through the
// recovered messaging.Service contract stored by Instance.
type lifecycleServiceAdapter struct {
	lifecycle IMSLifecycle
}

func (adapter lifecycleServiceAdapter) CancelUSSD(ctx context.Context, sessionID string) error {
	return adapter.lifecycle.CancelUSSD(ctx, sessionID)
}

func (adapter lifecycleServiceAdapter) ContinueUSSD(
	ctx context.Context,
	sessionID, input string,
) (*messaging.USSDResult, error) {
	return adapter.lifecycle.ContinueUSSD(ctx, sessionID, input)
}

func (adapter lifecycleServiceAdapter) GetSMSDeliveryStatus(
	messageID string,
) (*messaging.DeliveryStatus, error) {
	return adapter.lifecycle.GetSMSDeliveryStatus(context.Background(), messageID)
}

func (adapter lifecycleServiceAdapter) SetSMSMemoryFull(full bool) {
	if setter, ok := adapter.lifecycle.(interface{ SetSMSMemoryFull(bool) }); ok {
		setter.SetSMSMemoryFull(full)
	}
}

func (adapter lifecycleServiceAdapter) WakeMWISubscription() bool {
	waker, ok := adapter.lifecycle.(interface{ WakeMWISubscription() bool })
	return ok && waker.WakeMWISubscription()
}

func (adapter lifecycleServiceAdapter) SendSMSWithOptions(
	ctx context.Context,
	to, text string,
	options messaging.SendOptions,
) (messaging.SendOutcome, error) {
	return adapter.lifecycle.SendSMSWithOptions(ctx, to, text, options)
}

func (adapter lifecycleServiceAdapter) SendSMSWithResult(
	ctx context.Context,
	to, text string,
) (messaging.SendOutcome, error) {
	return adapter.lifecycle.SendSMSWithResult(ctx, to, text)
}

func (adapter lifecycleServiceAdapter) SendUSSD(
	ctx context.Context,
	code string,
) (*messaging.USSDResult, error) {
	return adapter.lifecycle.SendUSSD(ctx, code)
}

func (adapter lifecycleServiceAdapter) Status() map[string]interface{} {
	state := adapter.lifecycle.Status().State
	return map[string]interface{}{
		"device_id": state.DeviceID, "phase": state.Phase,
		"ims_ready": state.IMSReady, "sms_ready": state.SMSReady, "sms_mo_ready": state.SMSMOReady,
	}
}

func (adapter lifecycleServiceAdapter) StatusSnapshot() messaging.ServiceStatus {
	state := adapter.lifecycle.StatusSnapshot().State
	return messaging.ServiceStatus{
		Enabled: true, DeviceID: state.DeviceID, Registered: state.IMSReady,
		RegStatus: state.RegStatusText, State: state.Phase, RegState: state.IMSState,
	}
}

func (adapter lifecycleServiceAdapter) Stop(ctx context.Context) error {
	if withCtx, ok := adapter.lifecycle.(interface{ StopContext(context.Context) error }); ok {
		return withCtx.StopContext(ctx)
	}
	if adapter.lifecycle != nil {
		adapter.lifecycle.Stop()
	}
	return nil
}

func (adapter lifecycleServiceAdapter) TriggerRegisterImmediate(string) {
	_ = adapter.lifecycle.TriggerRegisterImmediate()
}

// imscoreLifecycleAdapter retains the explicitly injected current lifecycle API.
type imscoreLifecycleAdapter struct {
	svc *imscore.Service
}

func (adapter *imscoreLifecycleAdapter) Register(ctx context.Context) error {
	if adapter == nil || adapter.svc == nil {
		return errNoService
	}
	return adapter.svc.Register(ctx)
}

func (adapter *imscoreLifecycleAdapter) RegistrationErrors() <-chan error {
	if adapter == nil || adapter.svc == nil {
		return nil
	}
	return adapter.svc.RegistrationErrors()
}

func (adapter *imscoreLifecycleAdapter) SMSReadiness() SMSReadiness {
	if adapter == nil || adapter.svc == nil {
		return SMSReadiness{Reason: "IMS service is unavailable"}
	}
	return adaptSMSReadiness(adapter.svc.SMSReadiness())
}

func (adapter *imscoreLifecycleAdapter) SetOnSMSReadinessChanged(fn func(SMSReadiness)) {
	if adapter == nil || adapter.svc == nil {
		return
	}
	adapter.svc.SetOnSMSReadinessChanged(func(value imscore.SMSReadiness) {
		if fn != nil {
			fn(adaptSMSReadiness(value))
		}
	})
}

func (adapter *imscoreLifecycleAdapter) SetSMSMemoryFull(full bool) {
	if adapter != nil && adapter.svc != nil {
		adapter.svc.SetSMSMemoryFull(full)
	}
}

func (adapter *imscoreLifecycleAdapter) WakeMWISubscription() bool {
	return adapter != nil && adapter.svc.WakeMWISubscription()
}

func (adapter *imscoreLifecycleAdapter) SendSMSWithOptions(
	ctx context.Context,
	to, text string,
	options messaging.SendOptions,
) (messaging.SendOutcome, error) {
	return newServiceAdapter(adapter.svc).SendSMSWithOptions(ctx, to, text, options)
}

func (adapter *imscoreLifecycleAdapter) SendSMSWithResult(
	ctx context.Context,
	to, text string,
) (messaging.SendOutcome, error) {
	return newServiceAdapter(adapter.svc).SendSMSWithResult(ctx, to, text)
}

func (adapter *imscoreLifecycleAdapter) GetSMSDeliveryStatus(
	ctx context.Context,
	messageID string,
) (*messaging.DeliveryStatus, error) {
	return newServiceAdapter(adapter.svc).GetSMSDeliveryStatusContext(ctx, messageID)
}

func (adapter *imscoreLifecycleAdapter) SendUSSD(
	ctx context.Context,
	code string,
) (*messaging.USSDResult, error) {
	return newServiceAdapter(adapter.svc).SendUSSD(ctx, code)
}

func (adapter *imscoreLifecycleAdapter) ContinueUSSD(
	ctx context.Context,
	sessionID, input string,
) (*messaging.USSDResult, error) {
	return newServiceAdapter(adapter.svc).ContinueUSSD(ctx, sessionID, input)
}

func (adapter *imscoreLifecycleAdapter) CancelUSSD(ctx context.Context, sessionID string) error {
	return newServiceAdapter(adapter.svc).CancelUSSD(ctx, sessionID)
}

func (adapter *imscoreLifecycleAdapter) Status() Status {
	return newServiceAdapter(adapter.svc).StatusCurrent()
}

func (adapter *imscoreLifecycleAdapter) StatusSnapshot() Status { return adapter.Status() }

func (adapter *imscoreLifecycleAdapter) Stop() {
	_ = adapter.StopContext(context.Background())
}

func (adapter *imscoreLifecycleAdapter) StopContext(ctx context.Context) error {
	if adapter == nil || adapter.svc == nil {
		return nil
	}
	return adapter.svc.Stop(ctx)
}

func (adapter *imscoreLifecycleAdapter) TriggerRegisterImmediate() error {
	return newServiceAdapter(adapter.svc).TriggerRegisterImmediateCurrent()
}
