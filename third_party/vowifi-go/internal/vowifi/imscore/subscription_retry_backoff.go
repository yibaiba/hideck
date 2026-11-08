package imscore

import (
	"strings"
	"sync"
	"time"

	"github.com/iniwex5/vowifi-go/internal/vowifi/logging"
)

// Temporary SUBSCRIBE failures back off on their own schedule. Deriving the
// wait from the requested 600000-second Expires left a failed reg event usage
// idle for about 6.9 days (hideck#102). TS 24.229 5.1.1.3 and RFC 6665
// 4.1.2.4 do not say how soon to retry after a failed SUBSCRIBE or Timer N,
// so these steps are local pacing; the last step repeats.
var (
	registrationSubscriptionBackoff = []time.Duration{
		30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute,
	}
	mwiSubscriptionBackoff = []time.Duration{
		5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute,
	}
)

// SubscriptionRetryStore keeps the backoff per IMS identity and event package,
// apart from SubscriptionRegistrationStore: registration entries go away when
// bindings expire or the identity deregisters, and a runtime builds a new
// registration store on every start. The host keeps one retry store for the
// process and passes it to every runtime it starts, so the count survives
// Service rebuilds, runtime restarts and recoveries on both the default and
// the custom-tunnel path. It stays in memory, so a new process starts from the
// first step. The identity key includes the device ID.
type SubscriptionRetryStore struct {
	mu           sync.Mutex
	retries      map[string]subscriptionRetryState
	nextRevision uint64
}

func NewSubscriptionRetryStore() *SubscriptionRetryStore {
	return &SubscriptionRetryStore{retries: make(map[string]subscriptionRetryState)}
}

// SubscriptionRetries returns the store this Service records SUBSCRIBE
// failure backoff in.
func (s *Service) SubscriptionRetries() *SubscriptionRetryStore {
	return s.subscriptionRetries
}

type subscriptionRetryState struct {
	revision  uint64 // Store-wide sequence; deleting a record must not reuse it.
	failures  int
	localAt   time.Time // Local backoff.
	networkAt time.Time // Retry-After / retry-after; local pacing never shortens it.
	stopped   string    // MWI terminated with rejected, noresource or invariant.
	iccid     string    // Card the identity ran on, for the host's per-card reset.
}

func subscriptionBackoffDelay(eventPackage string, failures int) time.Duration {
	steps := registrationSubscriptionBackoff
	if eventPackage == mwiEventPackage {
		steps = mwiSubscriptionBackoff
	}
	return steps[min(max(failures, 1), len(steps))-1]
}

func (state subscriptionRetryState) retryAt(includeLocal bool) time.Time {
	if includeLocal && state.localAt.After(state.networkAt) {
		return state.localAt
	}
	return state.networkAt
}

func subscriptionRetryKey(identity, eventPackage string) string {
	return identity + "\x00" + eventPackage
}

func (store *SubscriptionRetryStore) subscriptionFailed(
	identity, eventPackage, iccid string,
	now time.Time,
	networkWait time.Duration,
) subscriptionRetryState {
	store.mu.Lock()
	defer store.mu.Unlock()
	key := subscriptionRetryKey(identity, eventPackage)
	state := store.retries[key]
	store.nextRevision++
	state.revision = store.nextRevision
	state.failures++
	state.iccid = iccid
	state.localAt = now.Add(subscriptionBackoffDelay(eventPackage, state.failures))
	if at := now.Add(networkWait); networkWait > 0 && at.After(state.networkAt) {
		state.networkAt = at
	}
	store.retries[key] = state
	return state
}

func (store *SubscriptionRetryStore) subscriptionRetry(identity, eventPackage string) subscriptionRetryState {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.retries[subscriptionRetryKey(identity, eventPackage)]
}

func (store *SubscriptionRetryStore) subscriptionStopped(identity, eventPackage, iccid, reason string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	key := subscriptionRetryKey(identity, eventPackage)
	state := store.retries[key]
	state.stopped = reason
	state.iccid = iccid
	store.retries[key] = state
}

type subscriptionConfirmation struct {
	revision uint64
	now      time.Time
}

// subscriptionConfirmed clears only failures visible when this attempt was
// sent. Overlapping Services cannot confirm each other's later failures;
// network waits and MWI stop states stay in force.
func (store *SubscriptionRetryStore) subscriptionConfirmed(identity, eventPackage string, confirmation subscriptionConfirmation) int {
	store.mu.Lock()
	defer store.mu.Unlock()
	key := subscriptionRetryKey(identity, eventPackage)
	state := store.retries[key]
	if state.revision != confirmation.revision {
		return 0 // This NOTIFY cannot confirm a failure recorded after its attempt.
	}
	if state.networkAt.After(confirmation.now) || state.stopped != "" {
		state.failures, state.localAt = 0, time.Time{}
		store.retries[key] = state
	} else {
		delete(store.retries, key)
	}
	return state.failures
}

// MWIBackoffReset reports what ResetMWILocalBackoff did for one identity.
type MWIBackoffReset struct {
	Device           string
	Failures         int       // Local failure count cleared.
	NetworkWaitUntil time.Time // Kept; zero when no network wait is running.
	Stopped          string    // Kept stop reason: rejected, noresource or invariant.
}

// ResetMWILocalBackoff clears the MWI failure count and local wait of every
// identity that ran on one card, on any device, when the user turns MWI on or
// asks to re-detect it (hideck#102). A network wait still running and a stop
// state stay, and reg event state is untouched, so the next SUBSCRIBE may
// still have to wait. sameCard compares ICCIDs the way the host does; it runs
// under the store lock and must not block.
func (store *SubscriptionRetryStore) ResetMWILocalBackoff(sameCard func(iccid string) bool) []MWIBackoffReset {
	store.mu.Lock()
	defer store.mu.Unlock()
	now := time.Now()
	var resets []MWIBackoffReset
	for key, state := range store.retries {
		if !strings.HasSuffix(key, "\x00"+mwiEventPackage) || state.iccid == "" || !sameCard(state.iccid) {
			continue
		}
		// trackSubscriptionRegistrationLocked starts every identity with the device ID.
		device, _, _ := strings.Cut(key, "\x00")
		reset := MWIBackoffReset{Device: device, Failures: state.failures, Stopped: state.stopped}
		if state.networkAt.After(now) {
			reset.NetworkWaitUntil = state.networkAt
		}
		state.failures, state.localAt = 0, time.Time{}
		if reset.NetworkWaitUntil.IsZero() && state.stopped == "" {
			delete(store.retries, key)
		} else {
			store.retries[key] = state
		}
		resets = append(resets, reset)
	}
	return resets
}

// WakeMWISubscription re-checks the MWI usage after the host reset its local
// backoff or the user turned MWI on. A retry still in the future follows the
// retry store: earlier once the local backoff is gone, never before a stored
// wait, and stopped if the identity's MWI was stopped, also by the other
// Service of an IKE reauthentication sharing the store. A retry already due,
// such as the immediate re-subscribe after deactivated or timeout, is left
// alone. A usage the policy stopped starts
// again. Sending still needs a registration, a policy that allows MWI and no
// stop state; an attempt already in flight is not repeated. It reports false
// only for a nil Service.
func (s *Service) WakeMWISubscription() bool {
	if s == nil {
		return false
	}
	now := time.Now()
	s.mu.Lock()
	f := s.subscriptionFieldsLocked(true)
	state := s.subscriptionRetries.subscriptionRetry(s.subscriptionBinding.identity, mwiEventPackage)
	if f.lifecycle.context == s.subscriptionContextLocked() && !f.retryBlocked() && f.lifecycle.retryAt.After(now) {
		if state.stopped != "" {
			f.lifecycle.blockedReason = state.stopped
		} else {
			f.scheduleRetry(laterSubscriptionRetry(now, state.retryAt(true)))
		}
	}
	retryAt, stopped := f.lifecycle.retryAt, firstNonBlank(f.lifecycle.blockedReason, state.stopped)
	registered := s.regState == regRegistered
	s.mu.Unlock()
	logging.Info("IMS SUBSCRIBE(mwi) re-check requested; sending may still wait",
		"device", s.DeviceID(), "registered", registered, "failures", state.failures,
		"retry_at", formatSubscriptionRetryAt(retryAt), "network_wait_until", formatSubscriptionRetryAt(state.networkAt),
		"stopped", stopped)
	if registered {
		s.startMWISubscription()
	}
	s.signalIMSMaintenance()
	return true
}

func formatSubscriptionRetryAt(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.Format(time.RFC3339)
}

// subscriptionFailureRetryLocked counts one temporary failure of f and returns
// the earliest time its next SUBSCRIBE may go out. networkWait is the wait the
// network asked for, zero when it gave none.
func (s *Service) subscriptionFailureRetryLocked(
	f subscriptionFields,
	binding subscriptionRegistration,
	now time.Time,
	networkWait time.Duration,
) time.Time {
	eventPackage := subscriptionEventPackage(f.mwi)
	state := s.subscriptionRetries.subscriptionFailed(binding.identity, eventPackage, s.cfg.ICCID, now, networkWait)
	at := state.retryAt(true)
	basis := "local backoff"
	if at.Equal(state.networkAt) {
		basis = "network wait"
	}
	logging.Info("IMS subscription retry scheduled",
		"device", s.DeviceID(), "event_package", eventPackage, "failures", state.failures,
		"retry_at", at.Format(time.RFC3339), "basis", basis, "reason", *f.lastErr)
	return at
}

// carrySubscriptionRetryLocked reapplies the identity's backoff to a usage
// that has just moved to a new registration context, where the in-memory
// retry time starts empty. MWI keeps its local backoff. The reg event package
// may subscribe once after a new initial registration (TS 24.229 5.1.1.3);
// a failure then continues from the current step. A REGISTER refresh that
// only changed the Service-Route is not an initial registration and gets no
// exception. Network waits apply to both.
//
// An MWI usage the notifier terminated with rejected, noresource or invariant
// stays stopped (RFC 6665 4.1.3: SHOULD NOT re-subscribe). The reg event
// package does not: TS 24.229 5.1.1.3 requires a subscription after every
// new initial registration.
func (s *Service) carrySubscriptionRetryLocked(f subscriptionFields, previous, current subscriptionContext) {
	state := s.subscriptionRetries.subscriptionRetry(current.binding.identity, subscriptionEventPackage(f.mwi))
	eventPackage := subscriptionEventPackage(f.mwi)
	if f.mwi && state.stopped != "" {
		f.lifecycle.blockedReason = state.stopped
		logging.Info("IMS subscription stays stopped in the new registration context",
			"device", s.DeviceID(), "event_package", eventPackage, "reason", state.stopped)
		return
	}
	refreshed := previous.binding == current.binding && previous.generation == current.generation
	exception := !f.mwi && !refreshed
	// Re-carrying an unchanged retry, e.g. on each REGISTER refresh while a
	// restarted MWI usage waits, logs nothing new.
	if at := state.retryAt(!exception); at.After(time.Now()) && !at.Equal(f.lifecycle.retryAt) {
		f.scheduleRetry(at)
		basis := "local backoff"
		if at.Equal(state.networkAt) {
			basis = "network wait"
		}
		logging.Info("IMS subscription retry carried into the new registration context",
			"device", s.DeviceID(), "event_package", eventPackage, "failures", state.failures,
			"retry_at", at.Format(time.RFC3339), "basis", basis, "initial_registration_exception", exception)
	}
}
