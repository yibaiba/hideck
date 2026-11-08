package imscore

import (
	"testing"
	"time"
)

func TestOldServiceNotifyPreservesNewServiceBackoff(t *testing.T) {
	for _, mwi := range []bool{false, true} {
		for _, state := range []string{"active", "pending"} {
			t.Run(subscriptionEventPackage(mwi)+"/"+state, func(t *testing.T) {
				retries := NewSubscriptionRetryStore()
				old := newRetryTestService(t, retries)
				initial := beginProtocolSubscription(t, old, mwi)
				completeProtocolSubscription(t, old, mwi, initial)
				current := newRetryTestService(t, retries)
				failSubscriptionByTimerN(t, current, mwi)
				before := retries.subscriptionRetry(current.subscriptionBinding.identity, subscriptionEventPackage(mwi))
				raw := notifyForSubscription(initial.request, state+";expires=120", 1)
				if _, status := old.acceptSubscriptionNotification(raw); status != 200 {
					t.Fatalf("old NOTIFY status = %d", status)
				}
				after := retries.subscriptionRetry(current.subscriptionBinding.identity, subscriptionEventPackage(mwi))
				if after != before {
					t.Fatalf("old NOTIFY changed another Service's retry: before=%+v after=%+v", before, after)
				}
				current.subscriptionFieldsLocked(mwi).lifecycle.retryAt = time.Time{}
				attempt := beginProtocolSubscription(t, current, mwi)
				raw = notifyForSubscription(attempt.request, state+";expires=120", 1)
				if _, status := current.acceptSubscriptionNotification(raw); status != 200 {
					t.Fatalf("current NOTIFY status = %d", status)
				}
				if got := subscriptionFailuresForTest(current, mwi); got != 0 {
					t.Fatalf("current attempt left %d failures", got)
				}
			})
		}
	}
}

func TestOldServiceNotifyPreservesBackoffAfterManualReset(t *testing.T) {
	retries := NewSubscriptionRetryStore()
	old := newRetryTestService(t, retries)
	old.cfg.ICCID = "card"
	failSubscriptionByTimerN(t, old, true)
	old.subscriptionFieldsLocked(true).lifecycle.retryAt = time.Time{}
	initial := beginProtocolSubscription(t, old, true)
	completeProtocolSubscription(t, old, true, initial)
	retries.ResetMWILocalBackoff(sameCardForTest("card"))
	current := newRetryTestService(t, retries)
	current.cfg.ICCID = "card"
	failSubscriptionByTimerN(t, current, true)
	before := retries.subscriptionRetry(current.subscriptionBinding.identity, mwiEventPackage)
	if _, status := old.acceptSubscriptionNotification(notifyForSubscription(initial.request, "active;expires=120", 1)); status != 200 {
		t.Fatalf("old NOTIFY status = %d", status)
	}
	if after := retries.subscriptionRetry(current.subscriptionBinding.identity, mwiEventPackage); after != before {
		t.Fatalf("deleted retry revision was reused: before=%+v after=%+v", before, after)
	}
}

func TestQueuedRefreshNotifyPreservesAnotherServicesBackoff(t *testing.T) {
	retries := NewSubscriptionRetryStore()
	old := newRetryTestService(t, retries)
	initial := beginProtocolSubscription(t, old, true)
	completeProtocolSubscription(t, old, true, initial)
	current := newRetryTestService(t, retries)
	failSubscriptionByTimerN(t, current, true)
	before := retries.subscriptionRetry(current.subscriptionBinding.identity, mwiEventPackage)
	queued := buildProtocolSubscription(t, old, true)
	if err := old.recordSubscriptionUsageAttempt(queued, true); err != nil {
		t.Fatal(err)
	}
	if _, status := old.acceptSubscriptionNotification(notifyForSubscription(initial.request, "active;expires=120", 1)); status != 200 {
		t.Fatalf("old NOTIFY status = %d", status)
	}
	if after := retries.subscriptionRetry(current.subscriptionBinding.identity, mwiEventPackage); after != before {
		t.Fatalf("queued refresh confirmed another Service's failure: before=%+v after=%+v", before, after)
	}
}
