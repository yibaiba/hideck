package imscore

import (
	"errors"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

// failSubscriptionByTimerN sends a SUBSCRIBE that gets a 200 but no NOTIFY and
// returns the delay Timer N scheduled for the next attempt.
func failSubscriptionByTimerN(t *testing.T, s *Service, mwi bool) (subscriptionResult, time.Duration) {
	t.Helper()
	f := s.subscriptionFieldsLocked(mwi)
	f.lifecycle.retryAt = time.Time{} // The previous wait has passed.
	result := beginProtocolSubscription(t, s, mwi)
	completeProtocolSubscription(t, s, mwi, result)
	deadline := f.lifecycle.notifyDeadline
	s.mu.Lock()
	s.expireSubscriptionTimersLocked(deadline)
	s.mu.Unlock()
	return result, f.lifecycle.retryAt.Sub(deadline)
}

func subscriptionFailuresForTest(s *Service, mwi bool) int {
	return s.subscriptionRegistrations.subscriptionRetry(
		s.subscriptionBinding.identity, subscriptionEventPackage(mwi)).failures
}

func TestSubscriptionTimerNBacksOffPerEventPackage(t *testing.T) {
	want := map[bool][]time.Duration{
		false: {30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute},
		true:  {5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute, 30 * time.Minute},
	}
	for mwi, steps := range want {
		s := newSubscriptionLifecycleTestService(t, nil)
		for i, step := range steps {
			if _, got := failSubscriptionByTimerN(t, s, mwi); got != step {
				t.Fatalf("mwi=%t failure %d waited %s, want %s", mwi, i+1, got, step)
			}
		}
	}
}

func TestSubscriptionBackoffClearsOnlyOnMatchingActiveOrPendingNotify(t *testing.T) {
	for _, mwi := range []bool{false, true} {
		for _, confirm := range []string{"active;expires=120", "pending;expires=120"} {
			s := newSubscriptionLifecycleTestService(t, nil)
			stale, _ := failSubscriptionByTimerN(t, s, mwi)
			f := s.subscriptionFieldsLocked(mwi)

			// A NOTIFY for the attempt Timer N already retired is not feedback
			// for the current subscription.
			if _, status := s.acceptSubscriptionNotification(notifyForSubscription(stale.request, confirm, 1)); status != 481 {
				t.Fatalf("late NOTIFY for a retired attempt got %d", status)
			}
			// Neither a REGISTER refresh nor a 200 to SUBSCRIBE confirms it.
			s.mu.Lock()
			s.trackSubscriptionRegistrationLocked(time.Hour)
			s.mu.Unlock()
			f.lifecycle.retryAt = time.Time{}
			result := beginProtocolSubscription(t, s, mwi)
			completeProtocolSubscription(t, s, mwi, result)
			if got := subscriptionFailuresForTest(s, mwi); got != 1 {
				t.Fatalf("mwi=%t: 200 or REGISTER cleared the backoff (failures=%d)", mwi, got)
			}
			// A terminated NOTIFY counts as another failure.
			if _, status := s.acceptSubscriptionNotification(notifyForSubscription(result.request, "terminated;reason=probation", 1)); status != 200 {
				t.Fatalf("terminated NOTIFY got %d", status)
			}
			if got := subscriptionFailuresForTest(s, mwi); got != 2 {
				t.Fatalf("mwi=%t: terminated NOTIFY left failures=%d", mwi, got)
			}

			f.lifecycle.retryAt = time.Time{}
			result = beginProtocolSubscription(t, s, mwi)
			if _, status := s.acceptSubscriptionNotification(notifyForSubscription(result.request, confirm, 1)); status != 200 {
				t.Fatalf("%s NOTIFY got %d", confirm, status)
			}
			if got := subscriptionFailuresForTest(s, mwi); got != 0 {
				t.Fatalf("mwi=%t: %s NOTIFY left failures=%d", mwi, confirm, got)
			}
			if _, got := failSubscriptionByTimerN(t, s, mwi); got != subscriptionBackoffDelay(subscriptionEventPackage(mwi), 1) {
				t.Fatalf("mwi=%t: backoff did not restart from the first step: %s", mwi, got)
			}
		}
	}
}

func TestSubscriptionRegisterRefreshDoesNotBypassBackoff(t *testing.T) {
	for _, mwi := range []bool{false, true} {
		s := newSubscriptionLifecycleTestService(t, nil)
		failSubscriptionByTimerN(t, s, mwi)
		s.mu.Lock()
		s.trackSubscriptionRegistrationLocked(time.Hour)
		s.mu.Unlock()
		if start, reason := s.prepareSubscriptionStart(mwi); start {
			t.Fatalf("mwi=%t: REGISTER refresh restarted the subscription early", mwi)
		} else if reason != "subscription retry is not due" {
			t.Fatalf("mwi=%t: unexpected skip reason %q", mwi, reason)
		}
	}
}

func TestSubscriptionServiceRouteRefreshGetsNoInitialRegistrationException(t *testing.T) {
	s := newSubscriptionLifecycleTestService(t, nil)
	failSubscriptionByTimerN(t, s, false)
	s.mu.Lock()
	s.serviceRoute = "<sip:other-scscf.example;lr>"
	s.mu.Unlock()
	if start, _ := s.prepareSubscriptionStart(false); start {
		t.Fatal("a Service-Route change on REGISTER refresh bypassed the reg event backoff")
	}
}

func TestSubscriptionTransactionTimeoutAndTimerNCountOnce(t *testing.T) {
	for _, mwi := range []bool{false, true} {
		s := newSubscriptionLifecycleTestService(t, nil)
		result := beginProtocolSubscription(t, s, mwi)
		result.err = errors.New("SUBSCRIBE transaction: context deadline exceeded")
		if mwi {
			_ = s.recordMWISubscriptionResult(result)
		} else {
			_ = s.recordSubscriptionResult(result)
		}
		f := s.subscriptionFieldsLocked(mwi)
		retry := f.lifecycle.retryAt
		if deadline := f.lifecycle.notifyDeadline; !deadline.IsZero() {
			s.mu.Lock()
			s.expireSubscriptionTimersLocked(deadline)
			s.mu.Unlock()
		}
		if got := subscriptionFailuresForTest(s, mwi); got != 1 {
			t.Fatalf("mwi=%t: one unanswered SUBSCRIBE counted %d failures", mwi, got)
		}
		if !f.lifecycle.retryAt.Equal(retry) {
			t.Fatalf("mwi=%t: Timer N moved the retry from %s to %s", mwi, retry, f.lifecycle.retryAt)
		}
	}
}

func TestSubscriptionConfirmationKeepsNetworkWait(t *testing.T) {
	store := NewSubscriptionRegistrationStore()
	s := newSubscriptionLifecycleTestService(t, store)
	initial := beginProtocolSubscription(t, s, false)
	completeProtocolSubscription(t, s, false, initial)
	_, _ = s.acceptSubscriptionNotification(notifyForSubscription(initial.request, "active;expires=120", 1))
	refresh := beginProtocolSubscription(t, s, false)
	refresh.response = sip.NewResponse(503, "Service Unavailable")
	refresh.response.AppendHeader(sip.NewHeader("Retry-After", "7200"))
	refresh.err = errors.New("503")
	_ = s.recordSubscriptionResult(refresh)
	// The original subscription is still valid and keeps notifying; that does
	// not confirm the failed refresh.
	if _, status := s.acceptSubscriptionNotification(notifyForSubscription(initial.request, "active;expires=100", 2)); status != 200 {
		t.Fatalf("NOTIFY on the still-valid subscription got %d", status)
	}
	if got := subscriptionFailuresForTest(s, false); got != 1 {
		t.Fatalf("NOTIFY on the older subscription cleared the failed refresh (failures=%d)", got)
	}
	rebuilt := newSubscriptionLifecycleTestService(t, store)
	if start, _ := rebuilt.prepareSubscriptionStart(false); start {
		t.Fatal("rebuild dropped the Retry-After wait")
	}
}

func TestSubscriptionBackoffSurvivesServiceRebuildAndBindingExpiry(t *testing.T) {
	store := NewSubscriptionRegistrationStore()
	old := newSubscriptionLifecycleTestService(t, store)
	failSubscriptionByTimerN(t, old, false)
	failSubscriptionByTimerN(t, old, false)
	failSubscriptionByTimerN(t, old, true)
	// The identity's registration entry goes away; the backoff must not.
	store.deregistered(old.subscriptionBinding, true)

	rebuilt := newSubscriptionLifecycleTestService(t, store)
	// TS 24.229 5.1.1.3: the reg event package subscribes after the new
	// initial registration. MWI keeps waiting for its local backoff.
	if start, reason := rebuilt.prepareSubscriptionStart(false); !start {
		t.Fatalf("reg event did not subscribe after a new initial registration: %s", reason)
	}
	if start, _ := rebuilt.prepareSubscriptionStart(true); start {
		t.Fatal("MWI skipped its local backoff after a Service rebuild")
	}
	// The one attempt fails again and continues from the third step.
	if _, got := failSubscriptionByTimerN(t, rebuilt, false); got != 2*time.Minute {
		t.Fatalf("rebuild restarted the reg event backoff: %s", got)
	}
	if got := subscriptionFailuresForTest(rebuilt, true); got != 1 {
		t.Fatalf("MWI failures after rebuild = %d", got)
	}

	// The backoff is in memory only; a new process starts from scratch.
	fresh := newSubscriptionLifecycleTestService(t, nil)
	if got := subscriptionFailuresForTest(fresh, false); got != 0 {
		t.Fatalf("new store inherited %d failures", got)
	}
}

func TestSubscriptionNetworkWaitIsNeverShortened(t *testing.T) {
	for _, mwi := range []bool{false, true} {
		store := NewSubscriptionRegistrationStore()
		s := newSubscriptionLifecycleTestService(t, store)
		result := beginProtocolSubscription(t, s, mwi)
		result.response = sip.NewResponse(503, "Service Unavailable")
		result.response.AppendHeader(sip.NewHeader("Retry-After", "7200"))
		result.err = errors.New("503")
		before := time.Now()
		if mwi {
			_ = s.recordMWISubscriptionResult(result)
		} else {
			_ = s.recordSubscriptionResult(result)
		}
		f := s.subscriptionFieldsLocked(mwi)
		if f.lifecycle.retryAt.Before(before.Add(7200 * time.Second)) {
			t.Fatalf("mwi=%t: local backoff shortened Retry-After: %s", mwi, f.lifecycle.retryAt.Sub(before))
		}
		// TS 24.229 5.1.2.2: the wait applies to the request, so neither a
		// new initial registration nor a rebuilt Service lifts it.
		rebuilt := newSubscriptionLifecycleTestService(t, store)
		if start, _ := rebuilt.prepareSubscriptionStart(mwi); start {
			t.Fatalf("mwi=%t: rebuild bypassed Retry-After", mwi)
		}

		// retry-after on a terminated NOTIFY is a minimum as well.
		s = newSubscriptionLifecycleTestService(t, nil)
		result = beginProtocolSubscription(t, s, mwi)
		before = time.Now()
		_, _ = s.acceptSubscriptionNotification(notifyForSubscription(result.request, "terminated;reason=giveup;retry-after=3600", 1))
		f = s.subscriptionFieldsLocked(mwi)
		if f.lifecycle.retryAt.Before(before.Add(time.Hour)) {
			t.Fatalf("mwi=%t: local backoff shortened retry-after", mwi)
		}
	}
}

func TestSubscriptionStopReasonsAcrossNewInitialRegistration(t *testing.T) {
	for _, reason := range []string{"rejected", "noresource", "invariant"} {
		for _, mwi := range []bool{false, true} {
			store := NewSubscriptionRegistrationStore()
			s := newSubscriptionLifecycleTestService(t, store)
			result := beginProtocolSubscription(t, s, mwi)
			_, _ = s.acceptSubscriptionNotification(notifyForSubscription(result.request, "terminated;reason="+reason, 1))
			rebuilt := newSubscriptionLifecycleTestService(t, store)
			start, _ := rebuilt.prepareSubscriptionStart(mwi)
			// RFC 6665 4.1.3 SHOULD NOT re-subscribe; for reg event, TS 24.229
			// 5.1.1.3 still requires a subscription after initial registration.
			if start == mwi {
				t.Fatalf("reason %s mwi=%t: start after new initial registration = %t", reason, mwi, start)
			}
		}
	}
}

func TestSubscriptionBackoffIsolatedByEventPackageAndIdentity(t *testing.T) {
	store := NewSubscriptionRegistrationStore()
	first := newSubscriptionLifecycleTestService(t, store)
	failSubscriptionByTimerN(t, first, false)
	failSubscriptionByTimerN(t, first, false)
	if got := subscriptionFailuresForTest(first, true); got != 0 {
		t.Fatalf("reg event failures leaked into MWI: %d", got)
	}

	other := newSubscriptionLifecycleTestService(t, store)
	other.mu.Lock()
	other.cfg.IMPI = "other-" + other.cfg.IMPI
	other.trackSubscriptionRegistrationLocked(time.Hour)
	other.mu.Unlock()
	if other.subscriptionBinding.identity == first.subscriptionBinding.identity {
		t.Fatal("test identities are not distinct")
	}
	if got := subscriptionFailuresForTest(other, false); got != 0 {
		t.Fatalf("another identity inherited %d failures", got)
	}
	if _, got := failSubscriptionByTimerN(t, other, false); got != 30*time.Second {
		t.Fatalf("another identity started at %s", got)
	}
}
