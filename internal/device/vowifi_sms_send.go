package device

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost"
	"github.com/iniwex5/vowifi-go/runtimehost/messaging"
	"github.com/yibaiba/hideck/internal/db"
	"github.com/yibaiba/hideck/internal/modem"
	"github.com/yibaiba/hideck/pkg/smscodec"
)

const (
	voWiFiSMSSendRecoveryTimeout = 90 * time.Second
	RoutedSMSViaVoWiFi           = "vowifi"
	RoutedSMSViaCS               = "cs"
)

type routedSMSSendResult struct {
	Via            string
	Outcome        messaging.SendOutcome
	FellBackToCS   bool
	PartsConfirmed int
	Destination    string
	Identity       SMSIdentity
	IdentityRead   bool
}

// ShouldFallbackVoWiFiSMSToCS reports whether IMS never accepted the MESSAGE.
func ShouldFallbackVoWiFiSMSToCS(outcome messaging.SendOutcome, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, messaging.ErrSMSNotReady) {
		return true
	}
	return outcome.RecommendCSFallback
}

func sendRoutedSMS(
	viaVoWiFi bool,
	sendVoWiFi func() (messaging.SendOutcome, error),
	sendCS func() error,
) (routedSMSSendResult, error) {
	if viaVoWiFi {
		if sendVoWiFi == nil {
			return routedSMSSendResult{Via: RoutedSMSViaVoWiFi}, errors.New("sms route: VoWiFi sender is unavailable")
		}
		outcome, err := sendVoWiFi()
		if err == nil {
			return routedSMSSendResult{Via: RoutedSMSViaVoWiFi, Outcome: outcome}, nil
		}
		if !ShouldFallbackVoWiFiSMSToCS(outcome, err) || sendCS == nil {
			return routedSMSSendResult{Via: RoutedSMSViaVoWiFi, Outcome: outcome}, err
		}
		if csErr := sendCS(); csErr != nil {
			if errors.Is(csErr, ErrSMSSelectorMismatch) {
				return routedSMSSendResult{Via: RoutedSMSViaVoWiFi, Outcome: outcome, FellBackToCS: true}, csErr
			}
			if result := csSMSResult(csErr); result.Outcome.DeliveryState != "" {
				result.FellBackToCS = true
				return result, csErr
			}
			return routedSMSSendResult{Via: RoutedSMSViaVoWiFi, Outcome: outcome, FellBackToCS: true}, err
		}
		return routedSMSSendResult{Via: RoutedSMSViaCS, Outcome: outcome, FellBackToCS: true}, nil
	}
	if sendCS == nil {
		return routedSMSSendResult{Via: RoutedSMSViaCS}, errors.New("sms route: CS sender is unavailable")
	}
	err := sendCS()
	return csSMSResult(err), err
}

func csSMSResult(err error) routedSMSSendResult {
	result := routedSMSSendResult{Via: RoutedSMSViaCS}
	var progress *modem.SMSSubmitError
	if errors.As(err, &progress) {
		result.Outcome.PartsTotal = progress.PartsTotal
		result.PartsConfirmed = progress.PartsConfirmed
	}
	switch {
	case errors.Is(err, modem.ErrSMSSubmitUnknown):
		result.Outcome.DeliveryState = db.SMSDeliveryStateUnknown
	case result.PartsConfirmed > 0:
		result.Outcome.DeliveryState = db.SMSDeliveryStatePartialAck
	}
	return result
}

// SendRoutedSMS sends via VoWiFi when that path is selected, and falls back to
// CS only when IMS never accepted the MESSAGE.
func (p *Pool) SendRoutedSMS(
	ctx context.Context,
	worker *Worker,
	phone, message string,
	opts smscodec.SubmitOptions,
) (routedSMSSendResult, error) {
	return p.SendRoutedSMSForIdentity(ctx, worker, phone, message, opts, SMSIdentity{})
}

// SendRoutedSMSForIdentity binds an explicit SIM selector to the selected worker through submission.
func (p *Pool) SendRoutedSMSForIdentity(
	ctx context.Context,
	worker *Worker,
	phone, message string,
	opts smscodec.SubmitOptions,
	selector SMSIdentity,
) (routedSMSSendResult, error) {
	if p == nil {
		return routedSMSSendResult{}, errors.New("sms route: pool is nil")
	}
	if worker == nil {
		return routedSMSSendResult{}, errors.New("sms route: worker is nil")
	}
	deviceID := worker.ID
	destination := strings.TrimSpace(phone)
	explicitSelector := selector != (SMSIdentity{})
	selector = normalizeSMSIdentity(selector)
	checkOwner := p.smsSendOwnerCheck(worker, selector)
	identity, err := p.ResolveSMSIdentity(deviceID)
	if err != nil {
		if explicitSelector && (errors.Is(err, ErrSMSIdentityUnknown) || errors.Is(err, ErrSMSIdentityConflict) || errors.Is(err, ErrSMSIdentityTransitioning)) {
			return routedSMSSendResult{}, ErrSMSSelectorMismatch
		}
		return routedSMSSendResult{}, fmt.Errorf("sms route: resolve SIM identity: %w", err)
	}
	if explicitSelector && !selector.matches(identity) {
		return routedSMSSendResult{}, ErrSMSSelectorMismatch
	}
	if err := checkOwner(); err != nil {
		return routedSMSSendResult{Identity: identity, IdentityRead: true}, err
	}
	request := outboundSMSRequest{Worker: worker, To: destination, Text: message, Options: opts}
	if explicitSelector {
		request.CheckOwner = checkOwner
	}
	if err := p.authorizeSMS(ctx, request); err != nil {
		return routedSMSSendResult{Identity: identity, IdentityRead: true}, err
	}
	p.mu.RLock()
	sendVoWiFiHook := p.routedVoWiFiSMSSend
	sendCSHook := p.routedCSSMSSend
	p.mu.RUnlock()
	result, err := sendRoutedSMS(
		p.ShouldRouteSMSViaVoWiFi(deviceID),
		func() (messaging.SendOutcome, error) {
			if sendVoWiFiHook != nil {
				release, err := p.acquireOutboundOperation(worker)
				if err != nil {
					return messaging.SendOutcome{}, err
				}
				defer release()
				if err := checkOwner(); err != nil {
					return messaging.SendOutcome{}, err
				}
				return sendVoWiFiHook(ctx, deviceID, destination, message, opts)
			}
			return p.sendVoWiFiSMS(ctx, voWiFiSMSSendRequest{
				DeviceID: deviceID, To: destination, Text: message, Check: checkOwner,
				Acquire: func() (func(), error) { return p.acquireOutboundOperation(worker) },
				Options: messaging.SendOptions{Encoding: string(opts.Encoding)},
			})
		},
		func() error {
			release, err := p.acquireOutboundOperation(worker)
			if err != nil {
				return err
			}
			defer release()
			if err := worker.confirmCSSMSIdentity(ctx, identity, checkOwner); err != nil {
				if explicitSelector && errors.Is(err, ErrSMSIdentityConflict) {
					return ErrSMSSelectorMismatch
				}
				return err
			}
			if sendCSHook != nil {
				return sendCSHook(deviceID, destination, message)
			}
			return worker.sendSMSWithOptions(destination, message, opts)
		},
	)
	result.Destination = destination
	result.Identity = identity
	result.IdentityRead = true
	return result, err
}

// AttachWorkerForTest registers a worker for tests in other packages.
func (p *Pool) AttachWorkerForTest(worker *Worker) {
	if p == nil || worker == nil {
		return
	}
	_ = p.registerWorkerStarting(worker)
}

// AttachWorkerWithSMSIdentityForTest registers a worker whose SIM identity is
// already stable, allowing external package tests to exercise outbound SMS.
func (p *Pool) AttachWorkerWithSMSIdentityForTest(worker *Worker, identity SMSIdentity) {
	if p == nil || worker == nil {
		return
	}
	worker.cacheMu.Lock()
	worker.state.Identity.ICCID = identity.ICCID
	worker.state.Identity.IMSI = identity.IMSI
	worker.state.Identity.Ready = true
	worker.state.Identity.Phase = simIdentityPhaseReady
	worker.cacheMu.Unlock()
	p.AttachWorkerForTest(worker)
}

// SetRoutedSMSTestSenders overrides the VoWiFi and CS legs of SendRoutedSMS for tests.
func (p *Pool) SetRoutedSMSTestSenders(
	sendVoWiFi func(context.Context, string, string, string, smscodec.SubmitOptions) (messaging.SendOutcome, error),
	sendCS func(string, string, string) error,
) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.routedVoWiFiSMSSend = sendVoWiFi
	p.routedCSSMSSend = sendCS
	p.mu.Unlock()
}

type voWiFiSMSRuntime interface {
	State() runtimehost.State
	SendSMSWithOptions(context.Context, string, string, messaging.SendOptions) (messaging.SendOutcome, error)
}

type voWiFiSMSSendRequest struct {
	Check    func() error
	Acquire  func() (func(), error)
	DeviceID string
	To       string
	Text     string
	Options  messaging.SendOptions
	Updates  <-chan runtimehost.State
	Runtime  func() voWiFiSMSRuntime
}

func (r voWiFiSMSSendRequest) checkOwner() error {
	if r.Check != nil {
		return r.Check()
	}
	return nil
}

func (r voWiFiSMSSendRequest) acquire() (func(), error) {
	if r.Acquire == nil {
		return func() {}, nil
	}
	return r.Acquire()
}

func sendVoWiFiSMSWhenReady(
	ctx context.Context,
	request voWiFiSMSSendRequest,
) (messaging.SendOutcome, error) {
	lastReason := "VoWiFi 运行时尚未建立"
	for {
		if err := request.checkOwner(); err != nil {
			return messaging.SendOutcome{}, err
		}
		runtime := currentVoWiFiSMSRuntime(request.Runtime)
		if runtime != nil {
			state := runtime.State()
			lastReason = voWiFiSMSWaitReason(state)
			if state.SMSMOReady || state.SMSReady || !shouldWaitForVoWiFiSMS(state) {
				release, err := request.acquire()
				if err != nil {
					return messaging.SendOutcome{}, err
				}
				if err := request.checkOwner(); err != nil {
					release()
					return messaging.SendOutcome{}, err
				}
				outcome, err := runtime.SendSMSWithOptions(ctx, request.To, request.Text, request.Options)
				release()
				if err == nil || !errors.Is(err, messaging.ErrSMSNotReady) {
					return outcome, err
				}
				lastReason = err.Error()
				if !shouldWaitForVoWiFiSMS(runtime.State()) {
					return outcome, err
				}
			}
		}
		if err := waitForVoWiFiSMSUpdate(ctx, request.Updates); err != nil {
			return messaging.SendOutcome{}, fmt.Errorf(
				"设备 %s 的 VoWiFi 等待短信恢复失败（最后状态：%s）: %w",
				request.DeviceID, lastReason, err,
			)
		}
	}
}

func currentVoWiFiSMSRuntime(getter func() voWiFiSMSRuntime) voWiFiSMSRuntime {
	if getter == nil {
		return nil
	}
	return getter()
}

func shouldWaitForVoWiFiSMS(state runtimehost.State) bool {
	return !state.SMSMOReady && !state.SMSReady && !strings.EqualFold(
		strings.TrimSpace(state.SMSReadyReason), "IMS SMSC is not configured",
	)
}

func voWiFiSMSWaitReason(state runtimehost.State) string {
	if reason := strings.TrimSpace(state.SMSReadyReason); reason != "" {
		return reason
	}
	if phase := strings.TrimSpace(state.Phase); phase != "" {
		return phase
	}
	return "VoWiFi 正在恢复"
}

func waitForVoWiFiSMSUpdate(ctx context.Context, updates <-chan runtimehost.State) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case _, ok := <-updates:
		if !ok {
			return errors.New("VoWiFi 状态订阅已关闭")
		}
		return nil
	}
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
