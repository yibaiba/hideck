package imscore

import (
	"sync"
	"time"

	"github.com/iniwex5/vowifi-go/internal/vowifi/logging"
)

// SubscriptionRejectionPersistence stores explicit 489 Bad Event responses.
// Ambiguous 405 responses, transport failures, and timeouts are scoped to the
// current registration and are not durable negative capability evidence.
type SubscriptionRejectionPersistence interface {
	LoadIMSSubscriptionRejection(identity, eventPackage string, now time.Time) (status int, expiresAt time.Time, err error)
	SaveIMSSubscriptionRejection(identity, eventPackage string, status int, expiresAt time.Time) error
	DeleteIMSSubscriptionRejections(identity string) error
}

// SubscriptionRegistrationStore follows IMS identity registration lifetimes,
// not TCP or Service lifetimes. It is shared across runtime recovery attempts.
type SubscriptionRegistrationStore struct {
	mu          sync.Mutex
	nextEpoch   uint64
	identities  map[string]*subscriptionRegistrationEntry
	persistence SubscriptionRejectionPersistence
}

type subscriptionRegistrationEntry struct {
	epoch      uint64
	bindings   map[string]time.Time
	rejections map[string]subscriptionRejection
}

type subscriptionRejection struct {
	status    int
	expiresAt time.Time
}

type subscriptionRegistration struct {
	identity string
	contact  string
	epoch    uint64
}

func NewSubscriptionRegistrationStore() *SubscriptionRegistrationStore {
	return &SubscriptionRegistrationStore{identities: make(map[string]*subscriptionRegistrationEntry)}
}

func NewPersistentSubscriptionRegistrationStore(
	persistence SubscriptionRejectionPersistence,
) *SubscriptionRegistrationStore {
	store := NewSubscriptionRegistrationStore()
	store.persistence = persistence
	return store
}

func (store *SubscriptionRegistrationStore) registered(binding subscriptionRegistration, expiresAt time.Time) subscriptionRegistration {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.identities == nil {
		store.identities = make(map[string]*subscriptionRegistrationEntry)
	}
	entry := store.activeLocked(binding.identity, time.Now())
	if entry == nil {
		entry = store.newEntryLocked(binding.identity, time.Now())
		store.identities[binding.identity] = entry
	}
	entry.bindings[binding.contact] = expiresAt
	binding.epoch = entry.epoch
	return binding
}

func (store *SubscriptionRegistrationStore) newEntryLocked(
	identity string,
	now time.Time,
) *subscriptionRegistrationEntry {
	store.nextEpoch++
	entry := &subscriptionRegistrationEntry{
		epoch: store.nextEpoch, bindings: make(map[string]time.Time),
		rejections: make(map[string]subscriptionRejection),
	}
	for _, eventPackage := range []string{registrationEventPackage, mwiEventPackage} {
		status, expiresAt, err := store.loadRejectionLocked(identity, eventPackage, now)
		if err == nil && now.Before(expiresAt) && persistentSubscriptionRejection(status) {
			entry.rejections[eventPackage] = subscriptionRejection{status: status, expiresAt: expiresAt}
		}
	}
	return entry
}

func (store *SubscriptionRegistrationStore) loadRejectionLocked(
	identity, eventPackage string,
	now time.Time,
) (int, time.Time, error) {
	if store.persistence == nil {
		return 0, time.Time{}, nil
	}
	status, expiresAt, err := store.persistence.LoadIMSSubscriptionRejection(identity, eventPackage, now)
	if err != nil {
		logging.WarnRate("ims-subscription-rejection-load-"+eventPackage, time.Minute,
			"IMS subscription rejection persistence load failed", "event_package", eventPackage, "err", err)
	}
	return status, expiresAt, err
}

func (store *SubscriptionRegistrationStore) activeLocked(identity string, now time.Time) *subscriptionRegistrationEntry {
	entry := store.identities[identity]
	if entry == nil {
		return nil
	}
	for contact, expiresAt := range entry.bindings {
		if !now.Before(expiresAt) {
			delete(entry.bindings, contact)
		}
	}
	if len(entry.bindings) == 0 {
		delete(store.identities, identity)
		return nil
	}
	return entry
}

func (store *SubscriptionRegistrationStore) reject(
	binding subscriptionRegistration,
	eventPackage string,
	status int,
) {
	if !identityScopedSubscriptionRejection(status) {
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	entry := store.activeLocked(binding.identity, time.Now())
	if entry != nil && entry.epoch == binding.epoch {
		expiresAt := latestBindingExpiry(entry)
		entry.rejections[eventPackage] = subscriptionRejection{status: status, expiresAt: expiresAt}
		if persistentSubscriptionRejection(status) {
			store.saveRejectionLocked(binding.identity, eventPackage, status, expiresAt)
		}
	}
}

func (store *SubscriptionRegistrationStore) rejected(
	binding subscriptionRegistration,
	eventPackage string,
) int {
	store.mu.Lock()
	defer store.mu.Unlock()
	entry := store.activeLocked(binding.identity, time.Now())
	if entry == nil || entry.epoch != binding.epoch {
		return 0
	}
	rejection := entry.rejections[eventPackage]
	if rejection.status == 0 {
		return 0
	}
	if !time.Now().Before(rejection.expiresAt) {
		delete(entry.rejections, eventPackage)
		return 0
	}
	return rejection.status
}

func (store *SubscriptionRegistrationStore) deregistered(binding subscriptionRegistration, all bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	entry := store.identities[binding.identity]
	if entry == nil || entry.epoch != binding.epoch {
		return
	}
	delete(entry.bindings, binding.contact)
	if all || len(entry.bindings) == 0 {
		delete(store.identities, binding.identity)
		store.deleteRejectionsLocked(binding.identity)
	}
}

func (store *SubscriptionRegistrationStore) saveRejectionLocked(
	identity, eventPackage string,
	status int,
	expiresAt time.Time,
) {
	if store.persistence == nil || expiresAt.IsZero() {
		return
	}
	if err := store.persistence.SaveIMSSubscriptionRejection(identity, eventPackage, status, expiresAt); err != nil {
		logging.WarnRate("ims-subscription-rejection-save-"+eventPackage, time.Minute,
			"IMS subscription rejection persistence save failed", "event_package", eventPackage, "err", err)
	}
}

func (store *SubscriptionRegistrationStore) deleteRejectionsLocked(identity string) {
	if store.persistence == nil {
		return
	}
	if err := store.persistence.DeleteIMSSubscriptionRejections(identity); err != nil {
		logging.WarnRate("ims-subscription-rejection-delete", time.Minute,
			"IMS subscription rejection persistence delete failed", "err", err)
	}
}

func latestBindingExpiry(entry *subscriptionRegistrationEntry) time.Time {
	latest := time.Time{}
	for _, expiresAt := range entry.bindings {
		if expiresAt.After(latest) {
			latest = expiresAt
		}
	}
	return latest
}

func persistentSubscriptionRejection(status int) bool {
	return status == 489
}

func identityScopedSubscriptionRejection(status int) bool {
	return status == 405 || persistentSubscriptionRejection(status)
}

func subscriptionEventPackage(mwi bool) string {
	if mwi {
		return mwiEventPackage
	}
	return registrationEventPackage
}
