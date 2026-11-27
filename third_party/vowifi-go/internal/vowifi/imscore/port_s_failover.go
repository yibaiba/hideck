package imscore

import (
	"fmt"
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/internal/vowifi/logging"
)

type portSFailoverCause struct {
	reason             string
	generation         uint64
	observedAt         time.Time
	deprioritizedUntil time.Time
}

func (cause portSFailoverCause) policy() string {
	switch cause.reason {
	case "downlink_validation_timeout":
		return "vodafone_uk_downlink_validation"
	case portSTransportTimeoutFailure:
		return "vodafone_uk_port_s_timeout"
	case vodafoneUKMTReportFailure:
		return vodafoneUKMTReportRecoveryPolicy
	default:
		return vodafoneUKPortSResetRecoveryPolicy
	}
}

// The caller owns registerMu and has validated the failure against the current
// binding. RST and timeout recovery share candidate selection, not detection.
func (s *Service) recoverPCSCFAfterPortSFailureLocked(failedRegistrar string, cause portSFailoverCause) {
	next, unavailableUntil, current := s.commitPortSFailover(failedRegistrar, cause)
	if !current {
		return
	}
	cause.deprioritizedUntil = unavailableUntil
	if next == "" {
		s.requestFreshRuntimeAfterPortSFailure(failedRegistrar, cause, "no alternate P-CSCF is available")
		return
	}
	s.recoverPortSOnAlternate(failedRegistrar, next, cause)
}

func (s *Service) commitPortSFailover(failedRegistrar string, cause portSFailoverCause) (string, time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Serialize the final failure check with accepting a recovered port-s.
	s.protectedConnMu.Lock()
	defer s.protectedConnMu.Unlock()
	if s.stopped() || strings.TrimSpace(s.registrar) != strings.TrimSpace(failedRegistrar) {
		return "", time.Time{}, false
	}
	if s.udpDownlinkProven.Load() {
		return "", time.Time{}, false
	}
	if s.hasCurrentTrackedPortSLocked(failedRegistrar) {
		return "", time.Time{}, false
	}
	if cause.generation != 0 &&
		!s.consumePortSTimeoutFailoverLocked(failedRegistrar, cause.generation, cause.reason) {
		return "", time.Time{}, false
	}
	// A new, confirmed transport failure is not an idle validation timeout.
	s.registrarPenalties.resetDownlinkRound(failedRegistrar)
	penalty := s.markVodafoneRegistrarFailure(failedRegistrar, cause.reason, nil)
	return s.advanceAvailableRegistrarLocked(), penalty.deprioritizedUntil, true
}

func (s *Service) recoverPortSOnAlternate(failedRegistrar, next string, cause portSFailoverCause) {
	s.markPCSCFRegistrationUnboundForPortSFailure(failedRegistrar, cause.reason)
	if err := s.resetRegistrationForPCSCFSwitch(); err != nil {
		s.reportRegistrationRuntimeError(err)
		return
	}
	logging.WarnRate("ims-ports-switch-"+s.DeviceID(), 30*time.Second,
		"IMS port-s recovery switching P-CSCF",
		"device", s.DeviceID(), "policy", cause.policy(), "reason", cause.reason,
		"previous", failedRegistrar, "next", next,
		"failure_at", cause.observedAt, "deprioritized_until", cause.deprioritizedUntil)
	baseline := s.captureDownlinkCheckpoint()
	ctx, cancel := s.recoveryRegistrationContext()
	err := s.registerLocked(ctx)
	cancel()
	if err != nil {
		s.rejectFailedPortSRegistrar(next, cause, err)
		return
	}
	if cause.reason == vodafoneUKMTReportFailure {
		s.settleMTReportRecoveryAfterRegister()
		logging.Info("IMS MT report recovery switched P-CSCF; awaiting SMSC redelivery",
			"device", s.DeviceID(), "policy", cause.policy(), "registrar", next)
		return
	}
	logging.Info("IMS port-s recovery registered; awaiting downlink validation",
		"device", s.DeviceID(), "policy", cause.policy(),
		"registrar", next, "timeout", s.portSFailoverValidationWait())
	validatedBy, ok := s.waitForPortSFailoverValidation(baseline)
	if !ok {
		s.watchReplacementDownlink(baseline, 0)
		return
	}
	logging.Info("IMS port-s recovery completed",
		"device", s.DeviceID(), "policy", cause.policy(),
		"registrar", next, "validated_by", validatedBy)
}

func (s *Service) waitForPortSFailoverValidation(baseline downlinkCheckpoint) (string, bool) {
	if !s.protectedDownlinkValidationRequired() {
		return "not_required", true
	}
	timer := time.NewTimer(s.portSFailoverValidationWait())
	defer timer.Stop()
	for {
		s.mu.RLock()
		evidence := s.downlinkEvidenceSinceLocked(baseline)
		s.mu.RUnlock()
		if evidence != "" {
			return evidence, true
		}
		select {
		case <-s.downlinkValidationWake:
		case <-timer.C:
			return "", false
		case <-s.stop:
			return "", false
		}
	}
}

func (s *Service) protectedDownlinkValidationRequired() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.protectedSMSPushRequiredLocked()
}

func (s *Service) portSFailoverValidationWait() time.Duration {
	if s != nil && s.portSFailoverVerifyWait > 0 {
		return s.portSFailoverVerifyWait
	}
	return defaultPortSDownlinkValidationWait
}

func (s *Service) signalDownlinkValidation() {
	if s == nil || s.downlinkValidationWake == nil {
		return
	}
	select {
	case s.downlinkValidationWake <- struct{}{}:
	default:
	}
}

func (s *Service) rejectFailedPortSRegistrar(registrar string, cause portSFailoverCause, err error) {
	penalty := s.markVodafoneRegistrarFailure(registrar, "initial_registration_failed", err)
	reason := fmt.Sprintf("initial registration failed: %v", err)
	cause.deprioritizedUntil = penalty.deprioritizedUntil
	s.requestFreshRuntimeAfterPortSFailure(registrar, cause, reason)
}

func (s *Service) failedRegisterUnavailableUntil(err error, now time.Time, failures uint32) time.Time {
	if failures == 0 {
		failures = 1
	}
	retryDelay := s.jitterPortSRecoveryDelay(rfc5626RecoveryUpperBound(failures, true))
	if retryAfter, present := registerRetryAfterFromError(err); present && retryAfter > retryDelay {
		retryDelay = retryAfter
	}
	return now.Add(retryDelay)
}

func (s *Service) requestFreshRuntimeAfterPortSFailure(registrar string, cause portSFailoverCause, reason string) {
	s.markPCSCFRegistrationUnboundForPortSFailure(registrar, cause.reason)
	err := fmt.Errorf("imscore: P-CSCF %s port-s recovery failed (%s); fresh runtime required", registrar, reason)
	logging.WarnRate("ims-ports-runtime-"+s.DeviceID(), 30*time.Second,
		"IMS port-s recovery requires a fresh runtime",
		"device", s.DeviceID(), "policy", cause.policy(),
		"pcscf", registrar, "reason", reason,
		"failure_at", cause.observedAt, "deprioritized_until", cause.deprioritizedUntil)
	s.reportRegistrationRuntimeError(err)
}
