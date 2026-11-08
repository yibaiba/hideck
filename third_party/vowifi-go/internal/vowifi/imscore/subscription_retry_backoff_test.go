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
	return s.subscriptionRetries.subscriptionRetry(
		s.subscriptionBinding.identity, subscriptionEventPackage(mwi)).failures
}

// newRetryTestService builds a Service the way a runtime restart does: a new
// registration store, and the retry store the host keeps for the process.
func newRetryTestService(t *testing.T, retries *SubscriptionRetryStore) *Service {
	t.Helper()
	s := newSubscriptionLifecycleTestService(t, nil)
	s.mu.Lock()
	s.subscriptionRetries = retries
	s.mu.Unlock()
	return s
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
	retries := NewSubscriptionRetryStore()
	s := newRetryTestService(t, retries)
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
	rebuilt := newRetryTestService(t, retries)
	if start, _ := rebuilt.prepareSubscriptionStart(false); start {
		t.Fatal("rebuild dropped the Retry-After wait")
	}
}

func TestSubscriptionBackoffSurvivesServiceRebuildAndBindingExpiry(t *testing.T) {
	retries := NewSubscriptionRetryStore()
	old := newRetryTestService(t, retries)
	failSubscriptionByTimerN(t, old, false)
	failSubscriptionByTimerN(t, old, false)
	failSubscriptionByTimerN(t, old, true)
	// The identity's registration entry goes away; the backoff must not.
	old.subscriptionRegistrations.deregistered(old.subscriptionBinding, true)

	rebuilt := newRetryTestService(t, retries)
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
		retries := NewSubscriptionRetryStore()
		s := newRetryTestService(t, retries)
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
		rebuilt := newRetryTestService(t, retries)
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
			retries := NewSubscriptionRetryStore()
			s := newRetryTestService(t, retries)
			result := beginProtocolSubscription(t, s, mwi)
			_, _ = s.acceptSubscriptionNotification(notifyForSubscription(result.request, "terminated;reason="+reason, 1))
			rebuilt := newRetryTestService(t, retries)
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
	retries := NewSubscriptionRetryStore()
	first := newRetryTestService(t, retries)
	failSubscriptionByTimerN(t, first, false)
	failSubscriptionByTimerN(t, first, false)
	if got := subscriptionFailuresForTest(first, true); got != 0 {
		t.Fatalf("reg event failures leaked into MWI: %d", got)
	}

	other := newRetryTestService(t, retries)
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

// During IKE reauthentication the old and the candidate Service run at once
// on the same store. A NOTIFY on the old MWI subscription must not lift the
// stop the candidate recorded (RFC 6665 4.1.3).
func TestSubscriptionConfirmationKeepsAnotherServicesStopState(t *testing.T) {
	retries := NewSubscriptionRetryStore()
	old := newRetryTestService(t, retries)
	initial := beginProtocolSubscription(t, old, true)
	candidate := newRetryTestService(t, retries)
	result := beginProtocolSubscription(t, candidate, true)
	_, _ = candidate.acceptSubscriptionNotification(notifyForSubscription(result.request, "terminated;reason=rejected", 1))
	if _, status := old.acceptSubscriptionNotification(notifyForSubscription(initial.request, "active;expires=120", 1)); status != 200 {
		t.Fatalf("NOTIFY on the old subscription got %d", status)
	}
	if start, _ := newRetryTestService(t, retries).prepareSubscriptionStart(true); start {
		t.Fatal("a NOTIFY on the old subscription lifted the MWI stop state")
	}
}

func sameCardForTest(iccid string) func(string) bool {
	return func(value string) bool { return value == iccid }
}

func newMWIResetTestService(t *testing.T, retries *SubscriptionRetryStore, deviceID, iccid string) *Service {
	t.Helper()
	s := newRetryTestService(t, retries)
	s.mu.Lock()
	s.cfg.DeviceID, s.cfg.ICCID = deviceID, iccid
	s.trackSubscriptionRegistrationLocked(time.Hour)
	s.mu.Unlock()
	return s
}

func failMWIWithStatusForTest(t *testing.T, s *Service, retryAfter string) {
	t.Helper()
	s.subscriptionFieldsLocked(true).lifecycle.retryAt = time.Time{}
	result := beginProtocolSubscription(t, s, true)
	result.response = sip.NewResponse(503, "Service Unavailable")
	if retryAfter != "" {
		result.response.AppendHeader(sip.NewHeader("Retry-After", retryAfter))
	}
	result.err = errors.New("503")
	_ = s.recordMWISubscriptionResult(result)
}

// The reset follows the card, including onto a device that held it before and
// is gone now, and leaves every other card and the reg event package alone.
func TestMWIBackoffResetClearsOnlyThisCardsLocalMWIBackoff(t *testing.T) {
	retries := NewSubscriptionRetryStore()
	card := newMWIResetTestService(t, retries, "wwan0", "8949020000000000001")
	failSubscriptionByTimerN(t, card, true)
	failSubscriptionByTimerN(t, card, true)
	failSubscriptionByTimerN(t, card, false)
	otherCard := newMWIResetTestService(t, retries, "wwan0", "8949020000000000002")
	otherCard.mu.Lock()
	otherCard.cfg.IMPI = "other-" + otherCard.cfg.IMPI
	otherCard.trackSubscriptionRegistrationLocked(time.Hour)
	otherCard.mu.Unlock()
	failSubscriptionByTimerN(t, otherCard, true)
	earlierDevice := newMWIResetTestService(t, retries, "wwan1", "8949020000000000001")
	failSubscriptionByTimerN(t, earlierDevice, true)
	otherDevice := newMWIResetTestService(t, retries, "wwan2", "8949020000000000003")
	failSubscriptionByTimerN(t, otherDevice, true)

	resets := retries.ResetMWILocalBackoff(sameCardForTest("8949020000000000001"))
	cleared := map[string]int{}
	for _, reset := range resets {
		if !reset.NetworkWaitUntil.IsZero() || reset.Stopped != "" {
			t.Fatalf("reset = %+v", reset)
		}
		cleared[reset.Device] = reset.Failures
	}
	if len(resets) != 2 || cleared["wwan0"] != 2 || cleared["wwan1"] != 1 {
		t.Fatalf("resets = %+v", resets)
	}
	for _, check := range []struct {
		name string
		s    *Service
		mwi  bool
		want int
	}{
		{"this card MWI", card, true, 0},
		{"this card reg event", card, false, 1},
		{"other card MWI", otherCard, true, 1},
		{"same card on an earlier device", earlierDevice, true, 0},
		{"other card on another device", otherDevice, true, 1},
	} {
		if got := subscriptionFailuresForTest(check.s, check.mwi); got != check.want {
			t.Fatalf("%s failures = %d, want %d", check.name, got, check.want)
		}
	}
	rebuilt := newMWIResetTestService(t, retries, "wwan0", "8949020000000000001")
	if start, reason := rebuilt.prepareSubscriptionStart(true); !start {
		t.Fatalf("MWI still waited after its local backoff was reset: %s", reason)
	}
	if _, got := failSubscriptionByTimerN(t, rebuilt, true); got != 5*time.Minute {
		t.Fatalf("MWI backoff after reset restarted at %s", got)
	}
}

func TestMWIBackoffResetKeepsNetworkWaitAndStopState(t *testing.T) {
	retries := NewSubscriptionRetryStore()
	waiting := newMWIResetTestService(t, retries, "wwan0", "8949020000000000001")
	failMWIWithStatusForTest(t, waiting, "7200")
	resets := retries.ResetMWILocalBackoff(sameCardForTest("8949020000000000001"))
	if len(resets) != 1 || resets[0].NetworkWaitUntil.Before(time.Now().Add(7100*time.Second)) {
		t.Fatalf("resets = %+v", resets)
	}
	if start, _ := newMWIResetTestService(t, retries, "wwan0", "8949020000000000001").prepareSubscriptionStart(true); start {
		t.Fatal("reset bypassed Retry-After")
	}

	retries = NewSubscriptionRetryStore()
	stopped := newMWIResetTestService(t, retries, "wwan0", "8949020000000000001")
	result := beginProtocolSubscription(t, stopped, true)
	_, _ = stopped.acceptSubscriptionNotification(notifyForSubscription(result.request, "terminated;reason=rejected", 1))
	resets = retries.ResetMWILocalBackoff(sameCardForTest("8949020000000000001"))
	if len(resets) != 1 || resets[0].Stopped == "" {
		t.Fatalf("resets = %+v", resets)
	}
	if start, _ := newMWIResetTestService(t, retries, "wwan0", "8949020000000000001").prepareSubscriptionStart(true); start {
		t.Fatal("reset lifted the MWI stop state")
	}
}

// holdMWIInFlightForTest marks a SUBSCRIBE(mwi) as in flight, so a wake that
// starts the usage does not reach the network in the test.
func holdMWIInFlightForTest(t *testing.T, s *Service) {
	t.Helper()
	s.mwiSubscriptionInFlight.Store(true)
	t.Cleanup(func() { s.mwiSubscriptionInFlight.Store(false) })
}

func TestWakeMWISubscriptionFollowsTheRetryStore(t *testing.T) {
	retries := NewSubscriptionRetryStore()
	s := newMWIResetTestService(t, retries, "wwan0", "8949020000000000001")
	failSubscriptionByTimerN(t, s, true)
	f := s.subscriptionFieldsLocked(true)
	local := f.lifecycle.retryAt
	holdMWIInFlightForTest(t, s)

	// Without a reset the local backoff still applies.
	s.WakeMWISubscription()
	if !f.lifecycle.retryAt.Equal(local) {
		t.Fatalf("wake moved the retry without a reset: %s -> %s", local, f.lifecycle.retryAt)
	}
	retries.ResetMWILocalBackoff(sameCardForTest("8949020000000000001"))
	s.WakeMWISubscription()
	if f.lifecycle.retryAt.After(time.Now()) || !f.lifecycle.started {
		t.Fatalf("wake after reset left MWI waiting until %s (started=%t)", f.lifecycle.retryAt, f.lifecycle.started)
	}
}

func TestWakeMWISubscriptionKeepsNetworkWaitAndStopState(t *testing.T) {
	retries := NewSubscriptionRetryStore()
	s := newMWIResetTestService(t, retries, "wwan0", "8949020000000000001")
	failMWIWithStatusForTest(t, s, "7200")
	holdMWIInFlightForTest(t, s)
	retries.ResetMWILocalBackoff(sameCardForTest("8949020000000000001"))
	s.WakeMWISubscription()
	if f := s.subscriptionFieldsLocked(true); f.lifecycle.retryAt.Before(time.Now().Add(7100 * time.Second)) {
		t.Fatalf("wake shortened Retry-After to %s", f.lifecycle.retryAt)
	}

	retries = NewSubscriptionRetryStore()
	s = newMWIResetTestService(t, retries, "wwan0", "8949020000000000002")
	holdMWIInFlightForTest(t, s)
	result := beginProtocolSubscription(t, s, true)
	_, _ = s.acceptSubscriptionNotification(notifyForSubscription(result.request, "terminated;reason=noresource", 1))
	retries.ResetMWILocalBackoff(sameCardForTest("8949020000000000002"))
	s.WakeMWISubscription()
	if f := s.subscriptionFieldsLocked(true); f.lifecycle.blockedReason == "" {
		t.Fatal("wake lifted the MWI stop state")
	}
	if start, _ := s.prepareSubscriptionStart(true); start {
		t.Fatal("stopped MWI started after a wake")
	}
}

// Finding B: a usage the SIM setting stopped stays stopped until the next
// registration unless the host wakes it after the user turns MWI on.
func TestWakeMWISubscriptionRestartsAUsageThePolicyStopped(t *testing.T) {
	policy := &fakeMWIPolicyStore{mode: MWISubscriptionOff}
	s := newMWIPolicyTestService(t, policy, false)
	if start, reason := s.prepareSubscriptionStart(true); !start {
		t.Fatal(reason)
	}
	s.stopBlockedMWIUsage(s.mwiSubscriptionBlockedReason())
	holdMWIInFlightForTest(t, s)
	s.WakeMWISubscription()
	if s.subscriptionFieldsLocked(true).lifecycle.started {
		t.Fatal("wake started MWI while the SIM setting is off")
	}
	policy.mode = MWISubscriptionOn
	s.WakeMWISubscription()
	if !s.subscriptionFieldsLocked(true).lifecycle.started {
		t.Fatal("wake did not restart MWI after the SIM setting turned on")
	}
}

// A reset while a SUBSCRIBE(mwi) is in flight sends nothing new, and the
// attempt's late failure counts from the reset instead of restoring the old
// count.
func TestMWIBackoffResetDuringAnAttemptInFlight(t *testing.T) {
	retries := NewSubscriptionRetryStore()
	s := newMWIResetTestService(t, retries, "wwan0", "8949020000000000001")
	failSubscriptionByTimerN(t, s, true)
	failSubscriptionByTimerN(t, s, true)
	f := s.subscriptionFieldsLocked(true)
	f.lifecycle.retryAt = time.Time{}
	inFlight := beginProtocolSubscription(t, s, true)
	holdMWIInFlightForTest(t, s)
	generation, attempt := f.lifecycle.usageGeneration, f.lifecycle.attemptKey

	retries.ResetMWILocalBackoff(sameCardForTest("8949020000000000001"))
	s.WakeMWISubscription()
	if f.lifecycle.usageGeneration != generation || f.lifecycle.attemptKey != attempt {
		t.Fatal("wake replaced the attempt in flight")
	}
	if start, reason := s.prepareSubscriptionStart(true); start {
		t.Fatalf("MWI could start again while an attempt is in flight (%s)", reason)
	}
	inFlight.response = sip.NewResponse(503, "Service Unavailable")
	inFlight.err = errors.New("503")
	before := time.Now()
	_ = s.recordMWISubscriptionResult(inFlight)
	if got := subscriptionFailuresForTest(s, true); got != 1 {
		t.Fatalf("late failure after reset left failures=%d", got)
	}
	if got := f.lifecycle.retryAt.Sub(before); got < 5*time.Minute || got > 5*time.Minute+time.Second {
		t.Fatalf("late failure after reset waits %s", got)
	}
}

func TestWakeMWISubscriptionWaitsForRegistration(t *testing.T) {
	retries := NewSubscriptionRetryStore()
	s := newMWIResetTestService(t, retries, "wwan0", "8949020000000000001")
	holdMWIInFlightForTest(t, s)
	s.mu.Lock()
	s.regState = regRegistering
	s.mu.Unlock()
	if !s.WakeMWISubscription() || s.subscriptionFieldsLocked(true).lifecycle.started {
		t.Fatal("wake started MWI without a registration")
	}
	var none *Service
	if none.WakeMWISubscription() {
		t.Fatal("nil Service reported a wake")
	}
}

// During IKE reauthentication two Services share the retry store. Waking one
// must apply the stop or the later network wait the other recorded.
func TestWakeMWISubscriptionFollowsAnotherServicesState(t *testing.T) {
	for _, stop := range []bool{false, true} {
		retries := NewSubscriptionRetryStore()
		waiting := newMWIResetTestService(t, retries, "wwan0", "8949020000000000001")
		other := newMWIResetTestService(t, retries, "wwan0", "8949020000000000001")
		result := beginProtocolSubscription(t, other, true)
		failSubscriptionByTimerN(t, waiting, true)
		if stop {
			_, _ = other.acceptSubscriptionNotification(notifyForSubscription(result.request, "terminated;reason=rejected", 1))
		} else {
			result.response = sip.NewResponse(503, "Service Unavailable")
			result.response.AppendHeader(sip.NewHeader("Retry-After", "7200"))
			result.err = errors.New("503")
			_ = other.recordMWISubscriptionResult(result)
		}
		holdMWIInFlightForTest(t, waiting)
		retries.ResetMWILocalBackoff(sameCardForTest("8949020000000000001"))
		waiting.WakeMWISubscription()
		f := waiting.subscriptionFieldsLocked(true)
		if stop && f.lifecycle.blockedReason == "" {
			t.Fatal("wake ignored the stop the other Service recorded")
		}
		if !stop && f.lifecycle.retryAt.Before(time.Now().Add(7100*time.Second)) {
			t.Fatalf("wake ignored the other Service's Retry-After: %s", f.lifecycle.retryAt)
		}
		if start, _ := waiting.prepareSubscriptionStart(true); start {
			t.Fatalf("stop=%t: MWI could start after the wake", stop)
		}
	}
}

// RFC 6665 4.1.3 lets a usage terminated with deactivated re-subscribe at
// once. A refresh 503 with Retry-After just before does not hold that back,
// and neither does a wake that lands before maintenance sends it.
func TestWakeMWISubscriptionKeepsAnImmediateResubscribe(t *testing.T) {
	retries := NewSubscriptionRetryStore()
	s := newMWIResetTestService(t, retries, "wwan0", "8949020000000000001")
	initial := beginProtocolSubscription(t, s, true)
	completeProtocolSubscription(t, s, true, initial)
	if _, status := s.acceptSubscriptionNotification(notifyForSubscription(initial.request, "active;expires=120", 1)); status != 200 {
		t.Fatalf("NOTIFY got %d", status)
	}
	failMWIWithStatusForTest(t, s, "7200") // A refresh gets 503; the dialog stays.
	if _, status := s.acceptSubscriptionNotification(notifyForSubscription(initial.request, "terminated;reason=deactivated", 2)); status != 200 {
		t.Fatalf("terminated NOTIFY got %d", status)
	}
	f := s.subscriptionFieldsLocked(true)
	if f.lifecycle.retryAt.After(time.Now()) {
		t.Fatalf("deactivated waited until %s", f.lifecycle.retryAt)
	}
	holdMWIInFlightForTest(t, s)
	retries.ResetMWILocalBackoff(sameCardForTest("8949020000000000001"))
	s.WakeMWISubscription()
	if f.lifecycle.retryAt.After(time.Now()) {
		t.Fatalf("wake moved the immediate re-subscribe to %s", f.lifecycle.retryAt)
	}
}
