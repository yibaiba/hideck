package imscore

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/emiago/sipgo/sip"
)

var errSubscriptionContextChanged = errors.New("imscore: subscription result belongs to a retired registration context")

type subscriptionContext struct {
	binding    subscriptionRegistration
	route      string
	generation uint64
}

type subscriptionLifecycle struct {
	context         subscriptionContext
	started         bool
	rejectedStatus  int
	blockedReason   string
	attemptKey      sipTransactionKey
	sentAt          time.Time
	notifyDeadline  time.Time
	expiresAt       time.Time
	retryAt         time.Time
	notifyExpires   bool
	initial         bool
	unsubscribing   bool
	notifyVersion   uint64
	notifications   *subscriptionNotificationQueue
	usageGeneration uint64
	selfRouted      bool
}

type subscriptionAttemptContext struct {
	subscriptionContext
	usageGeneration uint64
}

type subscriptionResult struct {
	context          subscriptionAttemptContext
	request          *sip.Request
	response         *sip.Response
	requestedExpires time.Duration
	unsubscribe      bool
	err              error
}

func (s *Service) subscriptionContextLocked() subscriptionContext {
	return subscriptionContext{
		binding:    s.subscriptionBinding,
		route:      s.serviceRoute,
		generation: s.subscriptionGeneration,
	}
}

func (s *Service) trackSubscriptionRegistrationLocked(expires time.Duration) {
	identity := firstNonBlank(s.regSession.publicID, s.reginfoAOR, primaryPublicIdentity(s.cfg))
	route := s.registeredSIPRouteLocked()
	contact, _ := registeredVoiceContact(s.cfg, firstNonBlank(s.regSession.contactUser, contactUser(s.cfg)), route.serverAddress)
	binding := subscriptionRegistration{
		identity: strings.Join([]string{s.DeviceID(), s.cfg.IMPI, s.cfg.Domain, identity}, "\x00"),
		contact:  contact,
	}
	s.subscriptionBinding = s.subscriptionRegistrations.registered(binding, time.Now().Add(expires))
}

// A normal REGISTER refresh preserves dialogs and explicit event-package
// rejections. A 405/489 is scoped to identity deregistration, not a TCP flow.
func (s *Service) prepareSubscriptionStart(mwi bool) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if eligible, reason := s.subscriptionGateLocked(); !eligible {
		return false, reason
	}
	lifecycle := s.alignSubscriptionContextLocked(mwi)
	current := lifecycle.context
	if status := s.subscriptionRegistrations.rejected(current.binding, subscriptionEventPackage(mwi)); status != 0 {
		if mwi {
			s.mwiSubscriptionClosed = true
			s.mwiSubscriptionRefreshAt = time.Time{}
			s.mwiSubscriptionLastErr = fmt.Sprintf("MWI rejected with status %d; waiting for IMS identity deregistration", status)
			return false, s.mwiSubscriptionLastErr
		}
		s.subscriptionClosed = true
		s.subscriptionRefreshAt = time.Time{}
		s.subscriptionLastErr = fmt.Sprintf("registration event subscription rejected with status %d; waiting for IMS identity deregistration", status)
		return false, s.subscriptionLastErr
	}
	if lifecycle.rejectedStatus != 0 {
		return false, fmt.Sprintf("subscription previously rejected with status %d", lifecycle.rejectedStatus)
	}
	if lifecycle.blockedReason != "" {
		return false, lifecycle.blockedReason
	}
	if lifecycle.unsubscribing {
		return false, "subscription is being removed"
	}
	if !lifecycle.retryAt.IsZero() && time.Now().Before(lifecycle.retryAt) {
		return false, "subscription retry is not due"
	}
	closed := s.subscriptionClosed
	if mwi {
		closed = s.mwiSubscriptionClosed
	}
	if lifecycle.started && !closed && s.subscriptionAttemptPendingLocked(mwi) {
		return false, "subscription lifecycle already started"
	}
	if closed {
		if mwi {
			s.resetMWISubscriptionLocked()
		} else {
			s.resetRegistrationSubscriptionLocked()
		}
	}
	lifecycle.started = true
	return true, ""
}

func (s *Service) alignSubscriptionContextLocked(mwi bool) *subscriptionLifecycle {
	current := s.subscriptionContextLocked()
	lifecycle := &s.subscriptionLifecycle
	if mwi {
		lifecycle = &s.mwiSubscriptionLifecycle
	}
	if lifecycle.context != current {
		previous := lifecycle.context
		if mwi {
			s.resetMWISubscriptionLocked()
		} else {
			s.resetRegistrationSubscriptionLocked()
		}
		*lifecycle = subscriptionLifecycle{context: current}
		s.carrySubscriptionRetryLocked(s.subscriptionFieldsLocked(mwi), previous, current)
	}
	return lifecycle
}

func (s *Service) subscriptionAttemptPendingLocked(mwi bool) bool {
	if mwi {
		return s.mwiSubscriptionInFlight.Load() || !s.mwiSubscriptionRefreshAt.IsZero() || s.mwiSubscriptionDialog.ready()
	}
	return s.subscriptionInFlight.Load() || !s.subscriptionRefreshAt.IsZero() || s.subscriptionDialog.ready()
}

func (s *Service) subscriptionResultCurrentLocked(result subscriptionResult) bool {
	// An in-place REGISTER refresh does not retire the existing subscription.
	registered := s.regState == regRegistered || s.regState == regRegistering
	return !s.stopped() && registered && result.context.subscriptionContext == s.subscriptionContextLocked()
}

func (s *Service) retrySubscriptionAfter481(result subscriptionResult, mwi bool) (subscriptionResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if result.response == nil || result.response.StatusCode != 481 || result.unsubscribe {
		return result, false
	}
	s.expireSubscriptionTimersLocked(time.Now())
	if s.validateSubscriptionAttemptLocked(result, mwi) != nil {
		return result, false
	}
	fields := s.subscriptionFieldsLocked(mwi)
	if !fields.dialog.ready() {
		return result, false
	}
	fields.terminate("SUBSCRIBE dialog rejected with 481")
	*fields.closed = false // The fresh attempt must also record failures before dispatch.
	return subscriptionResult{context: s.subscriptionAttemptContextLocked(mwi)}, true
}

func (s *Service) endSubscriptionRegistrationLocked(all bool) {
	s.subscriptionRegistrations.deregistered(s.subscriptionBinding, all)
	s.subscriptionGeneration++
	s.subscriptionLifecycle = subscriptionLifecycle{}
	s.mwiSubscriptionLifecycle = subscriptionLifecycle{}
}
