package imscore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/iniwex5/vowifi-go/internal/vowifi/events"
	"github.com/iniwex5/vowifi-go/internal/vowifi/logging"
)

const (
	mwiEventPackage        = "message-summary"
	mwiContentType         = "application/simple-message-summary"
	mwiSubscriptionTimeout = 10 * time.Second
	mwiSubscriptionFlow    = "subscribe_mwi"
)

func (s *Service) startMWISubscription() {
	if reason := s.mwiSubscriptionBlockedReason(); reason != "" {
		s.mu.RLock()
		active := s.mwiSubscriptionLifecycle.started
		s.mu.RUnlock()
		if active {
			// A usage from before the setting changed stops now, not at its
			// next refresh.
			s.stopBlockedMWIUsage(reason)
			return
		}
		logging.Info("IMS SUBSCRIBE(mwi) skipped",
			"device", s.DeviceID(), "reason", reason, "preset", s.cfg.CarrierPresetID)
		return
	}
	eligible, skipReason := s.prepareSubscriptionStart(true)
	if !eligible {
		logging.Info("IMS SUBSCRIBE(mwi) skipped",
			"device", s.DeviceID(), "reason", skipReason)
		return
	}
	logging.Info("IMS SUBSCRIBE(mwi) starting", "device", s.DeviceID())
	s.networkDone.Add(1)
	go func() {
		defer s.networkDone.Done()
		ctx, cancel := context.WithTimeout(context.Background(), mwiSubscriptionTimeout)
		defer cancel()
		if err := s.sendSubscribeMWI(ctx); err != nil {
			s.reportMWISubscriptionRuntimeError(err)
		}
	}()
}

func (s *Service) resetMWISubscriptionLocked() {
	s.mwiSubscriptionClosed = false
	s.mwiSubscriptionDialog = registrationSubscriptionDialog{}
	s.mwiSubscriptionRefreshAt = time.Time{}
	s.mwiSubscriptionExpires = 0
	s.mwiSubscriptionLastErr = ""
}

func (s *Service) reportMWISubscriptionRuntimeError(err error) {
	if err == nil || s.stopped() {
		return
	}
	if errors.Is(err, errSubscriptionContextChanged) || errors.Is(err, errSubscriptionUsageChanged) {
		logging.Debug("IMS SUBSCRIBE(mwi) retired attempt completed; checking current registration", "device", s.DeviceID(), "err", err)
		s.startMWISubscription()
		return
	}
	if !s.hasProtectedRegistrationTransport() {
		logging.Debug("IMS SUBSCRIBE(mwi) result discarded after registration changed",
			"device", s.DeviceID(), "err", err)
		return
	}
	logging.WarnRate("ims-subscribe-mwi-"+s.DeviceID(), 30*time.Second,
		"IMS SUBSCRIBE(mwi) failed; keeping current registration",
		"device", s.DeviceID(), "err", err)
}

func (s *Service) sendSubscribeMWI(ctx context.Context) error {
	return s.sendMWISubscription(ctx, registerExpires(s.cfg), false)
}

func (s *Service) sendUnsubscribeMWI(ctx context.Context) error {
	return s.sendMWISubscription(ctx, 0, true)
}

func (s *Service) unsubscribeMWI(ctx context.Context) {
	if s == nil || !s.hasMWISubscriptionDialog() {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	unsubCtx, cancel := context.WithTimeout(ctx, mwiSubscriptionTimeout)
	defer cancel()
	if err := s.sendUnsubscribeMWI(unsubCtx); err != nil {
		logging.Info("IMS SUBSCRIBE(mwi) unsubscribe failed",
			"device", s.DeviceID(), "err", err)
	}
}

func (s *Service) hasMWISubscriptionDialog() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mwiSubscriptionDialog.ready()
}

func (s *Service) sendMWISubscription(ctx context.Context, expires time.Duration, unsubscribe bool) error {
	if unsubscribe && !s.hasMWISubscriptionDialog() {
		return nil
	}
	if !s.mwiSubscriptionInFlight.CompareAndSwap(false, true) {
		if unsubscribe {
			return errors.New("imscore: MWI subscription is already in flight")
		}
		return nil
	}
	defer s.mwiSubscriptionInFlight.Store(false)
	s.subscribeMu.Lock()
	defer s.subscribeMu.Unlock()
	if unsubscribe && !s.hasMWISubscriptionDialog() {
		return nil
	}

	attempt, err := s.beginSubscriptionAttempt(true, unsubscribe)
	if err != nil {
		return err
	}
	result := subscriptionResult{context: attempt, unsubscribe: unsubscribe}
	request, requestedExpires, err := s.buildMWISubscription(expires)
	result.request, result.requestedExpires, result.err = request, requestedExpires, err
	if err != nil {
		return s.recordMWISubscriptionResult(result)
	}
	response, err := s.exchangeMWISubscription(ctx, result)
	result.response, result.err = response, err
	if blocked := (*mwiPolicyBlockedError)(nil); errors.As(err, &blocked) {
		s.stopBlockedMWIUsage(blocked.reason)
		return nil
	}
	if err != nil {
		return s.recordMWISubscriptionResult(result)
	}
	if retry, ok := s.retrySubscriptionAfter481(result, true); ok {
		result = retry
		logging.Info("IMS SUBSCRIBE(mwi) dialog gone; retrying as initial",
			"device", s.DeviceID())
		request, requestedExpires, err = s.buildMWISubscription(expires)
		result.request, result.requestedExpires, result.err = request, requestedExpires, err
		if err != nil {
			return s.recordMWISubscriptionResult(result)
		}
		response, err = s.exchangeMWISubscription(ctx, result)
		result.response, result.err = response, err
		if blocked := (*mwiPolicyBlockedError)(nil); errors.As(err, &blocked) {
			s.stopBlockedMWIUsage(blocked.reason)
			return nil
		}
		if err != nil {
			return s.recordMWISubscriptionResult(result)
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		result.err = fmt.Errorf("SUBSCRIBE rejected with status %d (%s)", response.StatusCode, response.Reason)
		return s.recordMWISubscriptionResult(result)
	}
	if err := s.recordMWISubscriptionResult(result); err != nil {
		return err
	}
	if unsubscribe {
		logging.Info("IMS SUBSCRIBE(mwi) unsubscribed", "call_id", request.CallID().Value())
		return nil
	}
	logging.Info("IMS SUBSCRIBE(mwi) succeeded", "call_id", request.CallID().Value(), "code", response.StatusCode)
	return nil
}

func (s *Service) exchangeMWISubscription(
	ctx context.Context,
	result subscriptionResult,
) (*sip.Response, error) {
	if err := s.recordMWISubscriptionAttempt(result); err != nil {
		return nil, err
	}
	request := result.request
	logging.Debug("IMS SUBSCRIBE(mwi) outbound", "device", s.DeviceID(), "sip", logging.RedactSIPRaw(request.String()))
	response, _, err := s.dispatchOutboundRequestWithCallbacks(outboundDispatchOptions{
		Context: ctx, Flow: mwiSubscriptionFlow, Request: request, Timeout: mwiSubscriptionTimeout,
		Callbacks: sipTransactionCallbacks{onBeforeSend: func() error {
			// Checked right before the bytes go out, so a setting saved while
			// this request waited for a lock or a dispatch slot still applies.
			if !result.unsubscribe {
				if reason := s.mwiSubscriptionBlockedReason(); reason != "" {
					return &mwiPolicyBlockedError{reason: reason}
				}
			}
			return s.subscriptionSent(result, true)
		}},
	}, true)
	if err != nil {
		return nil, fmt.Errorf("SUBSCRIBE transaction: %w", err)
	}
	return response, nil
}

func (s *Service) recordMWISubscriptionAttempt(result subscriptionResult) error {
	return s.recordSubscriptionUsageAttempt(result, true)
}

func (s *Service) recordMWISubscriptionResult(result subscriptionResult) error {
	return s.recordSubscriptionUsageResult(result, true)
}

func (s *Service) learnMWISubscriptionDialogLocked(request *sip.Request, response *sip.Response) {
	s.mwiSubscriptionDialog = learnSubscriptionDialog(s.mwiSubscriptionDialog, request, response)
}

func (s *Service) refreshMWISubscription() {
	ctx, cancel := context.WithTimeout(context.Background(), mwiSubscriptionTimeout)
	defer cancel()
	if err := s.sendSubscribeMWI(ctx); err != nil {
		s.reportMWISubscriptionRuntimeError(err)
	}
}

func mwiSubscriptionRefreshDelay(expires time.Duration) time.Duration {
	if expires > 2*imsSubscriptionRefreshAdvance {
		return expires - imsSubscriptionRefreshAdvance
	}
	return expires / 2
}

func sipEventPackage(raw string) string {
	event, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(rawSIPHeaderValue(raw, "Event"))), ";")
	return strings.TrimSpace(event)
}

func (s *Service) handleInboundNotification(raw string) {
	switch sipEventPackage(raw) {
	case mwiEventPackage:
		s.handleMWINotification(raw)
	case registrationEventPackage:
		s.handleRegistrationNotification(raw)
	default:
		logging.Info("IMS NOTIFY acknowledged", "event", rawSIPHeaderValue(raw, "Event"))
	}
}

func isMWINotification(raw string) bool {
	if sipEventPackage(raw) != mwiEventPackage {
		return false
	}
	contentType := strings.ToLower(strings.TrimSpace(rawSIPHeaderValue(raw, "Content-Type")))
	return contentType == "" || strings.Contains(contentType, "simple-message-summary")
}

func (s *Service) handleMWINotification(raw string) {
	notification, status := s.acceptSubscriptionNotification(raw)
	if status == 200 && notification.version != 0 {
		s.applySubscriptionNotificationBody(notification)
	}
}

func (s *Service) applyMWINotification(notification *subscriptionNotification) {
	raw := notification.raw
	logging.Info("IMS NOTIFY received", "event", mwiEventPackage)
	if !isMWINotification(raw) {
		return
	}
	body, err := rawSIPBody(raw)
	if err != nil {
		logging.WarnRate("ims-mwi-body", "IMS MWI body is invalid", "err", err)
		return
	}
	if len(body) == 0 {
		return
	}
	summary := parseMWISummary(string(body))
	s.mu.Lock()
	if !s.notificationBodyCurrentLocked(notification) {
		s.mu.Unlock()
		return
	}
	s.mwiLastSummary = summary.raw
	s.mwiMessagesWaiting = summary.waiting
	deviceID := ""
	if s.cfg != nil {
		deviceID = s.cfg.DeviceID
	}
	s.mu.Unlock()
	logging.Info("IMS MWI updated",
		"device", deviceID,
		"waiting", summary.waiting,
		"voice_new", summary.voiceNew,
		"voice_old", summary.voiceOld)
	s.publishRuntimeEvent(events.EventMWIUpdated{
		DevID:           deviceID,
		MessagesWaiting: summary.waiting,
		VoiceNew:        summary.voiceNew,
		VoiceOld:        summary.voiceOld,
		Account:         summary.account,
		Time:            time.Now(),
	})
}
