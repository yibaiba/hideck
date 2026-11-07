package imscore

import (
	"errors"
	"fmt"
	"time"

	"github.com/emiago/sipgo/sip"
)

const subscriptionTimerNMultiplier = 64

func (s *Service) recordSubscriptionUsageAttempt(result subscriptionResult, mwi bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireSubscriptionTimersLocked(time.Now())
	if err := s.validateSubscriptionAttemptLocked(result, mwi); err != nil {
		return err
	}
	f := s.subscriptionFieldsLocked(mwi)
	key, err := subscriptionRequestKey(result.request)
	if err != nil {
		return err
	}
	l := f.lifecycle
	l.context, l.started = result.context.subscriptionContext, true
	l.attemptKey, l.sentAt = key, time.Time{}
	l.selfRouted = false
	l.notifyDeadline, l.retryAt = time.Time{}, time.Time{}
	l.notifyExpires, l.unsubscribing = false, result.unsubscribe
	*f.closed, *f.lastErr = false, ""
	if result.request != nil {
		l.initial = toHeaderTag(result.request.To()) == ""
		// Reserve before dispatch: a failed new transaction still consumes its
		// CSeq. Transport retransmissions reuse the already-built request.
		if mwi {
			s.learnMWISubscriptionDialogLocked(result.request, nil)
		} else {
			s.learnSubscriptionDialogLocked(result.request, nil)
		}
	}
	return nil
}

func subscriptionRequestKey(request *sip.Request) (sipTransactionKey, error) {
	if request == nil {
		return sipTransactionKey{}, nil
	}
	return transactionKeyFromRequest(request.String())
}

func (s *Service) subscriptionSent(result subscriptionResult, mwi bool) error {
	t1 := s.transport.transactionTimers().t1
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireSubscriptionTimersLocked(time.Now())
	if err := s.validateSubscriptionAttemptLocked(result, mwi); err != nil {
		return err
	}
	f := s.subscriptionFieldsLocked(mwi)
	key, err := subscriptionRequestKey(result.request)
	if err != nil {
		return err
	}
	if f.lifecycle.attemptKey != key || *f.closed {
		return errSubscriptionUsageChanged
	}
	at := time.Now()
	f.lifecycle.sentAt = at
	// A NOTIFY received while this refresh was queued only updates the old
	// lifetime; it cannot override the not-yet-sent refresh's response.
	f.lifecycle.notifyExpires = false
	f.lifecycle.notifyDeadline = at.Add(subscriptionTimerNMultiplier * t1)
	*f.lastAttemptAt = at
	s.signalIMSMaintenance()
	return nil
}

func (s *Service) recordSubscriptionUsageResult(result subscriptionResult, mwi bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.signalIMSMaintenance()
	if !s.subscriptionResultCurrentLocked(result) {
		return errors.Join(errSubscriptionContextChanged, result.err)
	}
	s.expireSubscriptionTimersLocked(time.Now())
	f := s.subscriptionFieldsLocked(mwi)
	if errors.Is(result.err, errSubscriptionUsageChanged) {
		return result.err // An unsent retired request cannot change the usage.
	}
	if result.context.usageGeneration != f.lifecycle.usageGeneration {
		if result.err == nil && *f.closed {
			return nil // A terminal NOTIFY already completed this usage.
		}
		return errors.Join(errSubscriptionUsageChanged, result.err)
	}
	if result.err != nil {
		return s.failSubscriptionUsageLocked(f, result)
	}
	// A terminal NOTIFY or Timer N can precede the SUBSCRIBE final response.
	// That response must not resurrect the terminated usage or its timers.
	if *f.closed {
		return nil
	}
	if mwi {
		s.learnMWISubscriptionDialogLocked(result.request, result.response)
	} else {
		s.learnSubscriptionDialogLocked(result.request, result.response)
	}
	now := time.Now()
	*f.lastOKAt, *f.lastErr = now, ""
	if result.unsubscribe || result.requestedExpires <= 0 {
		*f.closed, *f.expires, *f.refreshAt = true, 0, time.Time{}
		f.lifecycle.expiresAt = time.Time{}
		return nil // Retain the dialog until its final NOTIFY or Timer N.
	}
	if !f.lifecycle.notifyExpires {
		f.setExpiry(now, subscriptionExpires(result.response, result.requestedExpires))
	}
	return nil
}

func (s *Service) failSubscriptionUsageLocked(f subscriptionFields, result subscriptionResult) error {
	if subscriptionPermanentlyRejected(result.response) {
		status := result.response.StatusCode
		if f.lifecycle.selfRouted && status == 405 {
			reason := fmt.Sprintf("SUBSCRIBE %s was routed back to the local contact", subscriptionEventPackage(f.mwi))
			f.terminate(reason)
			f.lifecycle.blockedReason = reason
			return fmt.Errorf("imscore: %s: %w", reason, result.err)
		}
		f.terminate(result.err.Error())
		if identityScopedSubscriptionRejection(status) {
			s.subscriptionRegistrations.reject(
				result.context.binding,
				subscriptionEventPackage(f.mwi),
				status,
			)
			if f.mwi {
				s.rememberMWIRejectionLocked(result.context.binding, status)
			}
		} else {
			f.lifecycle.rejectedStatus = status
		}
		return result.err
	}
	if *f.closed {
		return result.err
	}
	*f.lastErr = result.err.Error()
	if result.response != nil {
		// A failed final response does not promise a NOTIFY. Recoverable
		// refresh errors leave the previous negotiated expiration untouched.
		f.lifecycle.notifyDeadline = time.Time{}
		if subscriptionResponseTerminates(result.response.StatusCode) {
			f.terminate(result.err.Error())
		} else if !f.dialog.ready() {
			*f.dialog = registrationSubscriptionDialog{}
		}
	}
	if f.lifecycle.unsubscribing {
		return result.err
	}
	var networkWait time.Duration
	if result.response != nil {
		values := make([]string, 0, 1)
		for _, header := range result.response.GetHeaders("Retry-After") {
			values = append(values, header.Value())
		}
		retry, _, err := parseSIPRetryAfter(values)
		if err != nil {
			*f.lastErr = fmt.Sprintf("%v; %v", result.err, err)
		}
		networkWait = retry
	}
	f.scheduleRetry(s.subscriptionFailureRetryLocked(f, result.context.binding, time.Now(), networkWait))
	return result.err
}

func subscriptionResponseTerminates(status int) bool {
	if status >= 480 && status <= 485 {
		return true
	}
	switch status {
	case 404, 405, 410, 416, 489, 501, 604:
		return true
	default:
		return false
	}
}
