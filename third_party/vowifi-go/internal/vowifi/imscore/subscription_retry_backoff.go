package imscore

import (
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
	mu      sync.Mutex
	retries map[string]subscriptionRetryState
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
	failures  int
	localAt   time.Time // Local backoff.
	networkAt time.Time // Retry-After / retry-after; local pacing never shortens it.
	stopped   string    // MWI terminated with rejected, noresource or invariant.
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
	identity, eventPackage string,
	now time.Time,
	networkWait time.Duration,
) subscriptionRetryState {
	store.mu.Lock()
	defer store.mu.Unlock()
	key := subscriptionRetryKey(identity, eventPackage)
	state := store.retries[key]
	state.failures++
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

func (store *SubscriptionRetryStore) subscriptionStopped(identity, eventPackage, reason string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	key := subscriptionRetryKey(identity, eventPackage)
	state := store.retries[key]
	state.stopped = reason
	store.retries[key] = state
}

// subscriptionConfirmed ends the local backoff and returns the failures it
// cleared. A network wait that has not run out stays in force, and so does an
// MWI stop state: during IKE reauthentication the old and the new Service
// share this store, and a NOTIFY on the old subscription must not lift the
// stop the new one recorded.
func (store *SubscriptionRetryStore) subscriptionConfirmed(identity, eventPackage string, now time.Time) int {
	store.mu.Lock()
	defer store.mu.Unlock()
	key := subscriptionRetryKey(identity, eventPackage)
	state := store.retries[key]
	if state.networkAt.After(now) || state.stopped != "" {
		store.retries[key] = subscriptionRetryState{networkAt: state.networkAt, stopped: state.stopped}
	} else {
		delete(store.retries, key)
	}
	return state.failures
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
	state := s.subscriptionRetries.subscriptionFailed(binding.identity, eventPackage, now, networkWait)
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
