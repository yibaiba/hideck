package imscore

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/internal/vowifi/logging"
)

const pcscfInitialRegistrationTimeout = 30 * time.Second

type pcscf503RecoveryDecision struct {
	recover          bool
	retryAfter       time.Duration
	unavailableUntil time.Time
	headerInvalid    bool
}

type pcscf503RecoveryInput struct {
	response       *sipResponse
	timerB         time.Duration
	now            time.Time
	defaultPenalty time.Duration
}

func decidePCSCF503Recovery(input pcscf503RecoveryInput) pcscf503RecoveryDecision {
	if input.response == nil || input.response.StatusCode != 503 {
		return pcscf503RecoveryDecision{}
	}
	retryAfter, present, err := parseSIPRetryAfter(input.response.HeaderValues("Retry-After"))
	if err != nil || !present {
		if input.defaultPenalty <= 0 {
			input.defaultPenalty = rfc5626RecoveryBaseAllFailed
		}
		return pcscf503RecoveryDecision{
			recover: true, unavailableUntil: input.now.Add(input.defaultPenalty), headerInvalid: err != nil,
		}
	}
	if input.timerB <= 0 {
		input.timerB = defaultSIPTransactionTimers().bf
	}
	if retryAfter <= input.timerB {
		return pcscf503RecoveryDecision{retryAfter: retryAfter}
	}
	// IR.92 2.2.1 only requires a new initial registration when 503 has no
	// Retry-After. Waiting longer than Timer B would strand the INVITE, so
	// this host still fails over and remembers the P-CSCF as unavailable
	// until Retry-After elapses.
	return pcscf503RecoveryDecision{
		recover: true, retryAfter: retryAfter, unavailableUntil: input.now.Add(retryAfter),
	}
}

func parseSIPRetryAfter(values []string) (time.Duration, bool, error) {
	if len(values) == 0 {
		return 0, false, nil
	}
	value := strings.TrimSpace(values[0])
	if end := strings.IndexAny(value, " \t(;"); end >= 0 {
		value = value[:end]
	}
	seconds, err := strconv.ParseInt(value, 10, 32)
	if err != nil || seconds < 0 {
		return 0, true, fmt.Errorf("invalid SIP Retry-After %q", values[0])
	}
	return time.Duration(seconds) * time.Second, true, nil
}

func (s *Service) observeInitialInvite503(response *sipResponse) {
	penalty := s.jitterPortSRecoveryDelay(rfc5626RecoveryUpperBound(0, true))
	decision := decidePCSCF503Recovery(pcscf503RecoveryInput{
		response: response, timerB: s.inviteTimerB(), now: time.Now(), defaultPenalty: penalty,
	})
	if !decision.recover {
		return
	}
	if decision.headerInvalid {
		logging.Info("IMS 503 has invalid Retry-After; treating it as absent",
			"device", s.DeviceID(), "retry_after", response.Header("Retry-After"))
	}
	if !s.pcscfRecoveryPending.CompareAndSwap(false, true) {
		return
	}
	s.mu.RLock()
	failedRegistrar := s.registrar
	s.mu.RUnlock()
	if !s.startPCSCFRecovery(func() { s.recoverPCSCFAfter503(failedRegistrar, decision) }) {
		s.pcscfRecoveryPending.Store(false)
	}
}

// startPCSCFRecovery runs a P-CSCF recovery that registers again, which may
// run AKA on the SIM, so StopCurrent can wait for it. It is counted under
// s.mu: StopCurrent closes stop and then takes s.mu before it waits, so a
// recovery is either counted or never started.
func (s *Service) startPCSCFRecovery(run func()) bool {
	s.mu.Lock()
	if s.stopped() {
		s.mu.Unlock()
		return false
	}
	s.pcscfRecoveryWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.pcscfRecoveryWG.Done()
		run()
	}()
	return true
}

// recoveryRegistrationContext bounds a recovery registration and cancels it
// when the service stops.
func (s *Service) recoveryRegistrationContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), pcscfInitialRegistrationTimeout)
	go func() {
		select {
		case <-s.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func (s *Service) inviteTimerB() time.Duration {
	if s != nil && s.transport != nil && s.transport.timers.bf > 0 {
		return s.transport.timers.bf
	}
	return defaultSIPTransactionTimers().bf
}

func (s *Service) recoverPCSCFAfter503(
	failedRegistrar string,
	decision pcscf503RecoveryDecision,
) {
	defer s.finishPCSCFRecovery()
	s.registerMu.Lock()
	defer s.registerMu.Unlock()
	if s.stopped() {
		return
	}
	next, current := s.markRegistrarUnavailableAndAdvance(
		failedRegistrar, decision.unavailableUntil,
	)
	if !current {
		return
	}
	s.markPCSCFRegistrationUnbound(failedRegistrar)
	if err := s.resetRegistrationForPCSCFSwitch(); err != nil {
		s.reportRegistrationRuntimeError(err)
		return
	}
	if next == "" {
		s.reportRegistrationRuntimeError(fmt.Errorf(
			"imscore: P-CSCF %s returned 503 and no alternate is available", failedRegistrar,
		))
		return
	}
	logging.Info("IMS P-CSCF recovery starting initial registration",
		"device", s.DeviceID(), "previous", failedRegistrar, "next", next,
		"retry_after_seconds", int(decision.retryAfter/time.Second))
	ctx, cancel := s.recoveryRegistrationContext()
	defer cancel()
	if err := s.registerLocked(ctx); err != nil {
		s.reportRegistrationRuntimeError(fmt.Errorf("imscore: alternate P-CSCF registration failed: %w", err))
		return
	}
	logging.Info("IMS P-CSCF recovery completed", "device", s.DeviceID(), "registrar", next)
}

func (s *Service) markPCSCFRegistrationUnbound(registrar string) {
	reason := fmt.Sprintf("P-CSCF %s returned 503 Service Unavailable", registrar)
	s.markPCSCFRegistrationUnboundWithReason(reason, 503, "503 Service Unavailable")
}

func (s *Service) markPCSCFRegistrationUnboundForPortSFailure(registrar, failure string) {
	reason := fmt.Sprintf("P-CSCF %s protected port-s recovery failed: %s", registrar, failure)
	s.markPCSCFRegistrationUnboundWithReason(reason, 0, failure)
}

func (s *Service) markPCSCFRegistrationUnboundWithReason(reason string, sipCode int32, sipText string) {
	s.mu.Lock()
	s.regState = regFailed
	s.signalingReady = false
	s.signalingFailureReason = reason
	s.registrationRefreshAt = time.Time{}
	s.lastSIPText = sipText
	s.mu.Unlock()
	s.lastSIPCode.Store(sipCode)
	s.reRegisterPending.Store(true)
	s.transitionRegStatus(registrationRejectedTemporary)
	s.notifySMSReadiness()
}

func (s *Service) resetRegistrationForPCSCFSwitch() error {
	s.retireReplacedSecurity(s.saRetirement.Load(), "pcscf_switch")
	s.resetRegistrationTransportForRegistrarRetry()
	for _, conn := range s.detachProtectedConnections() {
		s.markPortSLocalClose(conn)
		_ = conn.Close()
	}
	if err := s.removeInstalledIPSec3GPP(); err != nil {
		return fmt.Errorf("imscore: remove old P-CSCF IPsec policy: %w", err)
	}
	s.mu.Lock()
	s.regSession = nil
	s.subscriptionGeneration++
	s.serviceRoute = ""
	s.path = ""
	s.pubGRUU = ""
	s.tempGRUU = ""
	s.learnedAOR = ""
	s.reginfoAOR = ""
	s.securityVerify = ""
	s.subscriptionRefreshAt = time.Time{}
	s.subscriptionLastAttemptAt = time.Time{}
	s.subscriptionLastOKAt = time.Time{}
	s.subscriptionLastErr = ""
	s.subscriptionClosed = false
	s.subscriptionDialog = registrationSubscriptionDialog{}
	s.mwiSubscriptionRefreshAt = time.Time{}
	s.mwiSubscriptionLastAttemptAt = time.Time{}
	s.mwiSubscriptionLastOKAt = time.Time{}
	s.mwiSubscriptionLastErr = ""
	s.mwiSubscriptionClosed = false
	s.mwiSubscriptionDialog = registrationSubscriptionDialog{}
	s.sipOutboundKeepalive = false
	s.sipOutbound = false
	s.sipOutboundFlow.Store(false)
	s.sipOutboundRequired = false
	s.outboundContactOffered = false
	s.outboundContactRegistered = false
	s.flowTimer = 0
	s.stunMappedAddr = nil
	s.mu.Unlock()
	s.lastRegisterContactCount.Store(0)
	return nil
}
