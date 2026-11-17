package imscore

import (
	"errors"
	"testing"
	"time"

	"github.com/iniwex5/vowifi-go/internal/vowifi/events"
	"github.com/iniwex5/vowifi-go/internal/vowifi/smsdelivery"
)

func TestMixedSMSCenterFragmentsStillRejectCollisions(t *testing.T) {
	for _, field := range []string{"total", "content"} {
		t.Run(field, func(t *testing.T) {
			service, sub := newFragmentPersistenceService(t, newMemoryInboundFragmentStore())
			first := persistedFragmentMessage(1, "original")
			if _, err := service.finalizeInboundSMSData(fragmentTestRequest("original"), first, "SIP/2.0 200 OK\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			conflict := first
			conflict.serviceCenter = "+12025550102"
			if field == "total" {
				conflict.total = 3
			} else {
				conflict.content = "conflict"
			}
			_, err := service.finalizeInboundSMSData(fragmentTestRequest("conflict"), conflict, "SIP/2.0 200 OK\r\n\r\n")
			if !errors.Is(err, smsdelivery.ErrInboundFragmentCollision) {
				t.Fatalf("collision not rejected: %v", err)
			}
			assertNoIMSEvent(t, sub, "conflicting "+field)
		})
	}
}

// All payloads and identities below are synthetic test fixtures.
// A carrier may route adjacent segments through different SMS centers. They
// must still produce one complete event, in either arrival order and storage mode.
func TestMultipartSMSAssemblesAcrossSMSCenters(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		for _, reversed := range []bool{false, true} {
			name := "volatile"
			if persistent {
				name = "persisted"
			}
			if reversed {
				name += "/reversed"
			}
			t.Run(name, func(t *testing.T) {
				bus := NewEventBus()
				sub := &captureIMSEventSubscriber{events: make(chan events.Event, 4)}
				bus.Subscribe(sub)
				cfg := &IMSConfig{DeviceID: "test-modem", IMSI: "001010000000001", ICCID: "test-card", EventBus: bus}
				if persistent {
					cfg.DeliveryStore = newMemoryInboundFragmentStore()
				}
				service, err := New(cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(service.StopCurrent)
				first := persistedFragmentMessage(1, "Simulated SMS part 1. ")
				first.sender = "TestSender"
				first.serviceCenter = "+12025550101"
				second := persistedFragmentMessage(2, "Simulated SMS part 2.")
				second.sender = "TestSender"
				second.serviceCenter = "+12025550102"
				parts := []inboundSMS{first, second}
				if reversed {
					parts = []inboundSMS{second, first}
				}
				for i, part := range parts {
					if _, err := service.finalizeInboundSMSData(fragmentTestRequest("smsc-route"), part, "SIP/2.0 200 OK\r\n\r\n"); err != nil {
						t.Fatal(err)
					}
					if i == 0 {
						assertNoIMSEvent(t, sub, "only one segment")
					}
				}
				select {
				case event := <-sub.events:
					received, ok := event.(*events.EventSMSReceived)
					if !ok || received.Content != "Simulated SMS part 1. Simulated SMS part 2." || received.Sender != "TestSender" || received.Incomplete {
						t.Fatalf("complete event=%#v", event)
					}
				case <-time.After(time.Second):
					t.Fatal("both segments arrived but no complete SMS event")
				}
				second.serviceCenter = "+12025550103"
				if _, err := service.finalizeInboundSMSData(fragmentTestRequest("smsc-retry"), second, "SIP/2.0 200 OK\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
				assertNoIMSEvent(t, sub, "duplicate segment via another SMS center")
			})
		}
	}
}

func TestMixedSMSCenterFragmentsRecoverAfterRestart(t *testing.T) {
	store := newMemoryInboundFragmentStore()
	firstService, _ := newFragmentPersistenceService(t, store)
	first := persistedFragmentMessage(1, "first ")
	first.serviceCenter = "+12025550101"
	if _, err := firstService.finalizeInboundSMSData(fragmentTestRequest("before-restart"), first, "SIP/2.0 200 OK\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	firstService.StopCurrent()
	secondService, sub := newFragmentPersistenceService(t, store)
	if err := secondService.restoreInboundFragments(); err != nil {
		t.Fatal(err)
	}
	second := persistedFragmentMessage(2, "second")
	second.serviceCenter = "+12025550102"
	if _, err := secondService.finalizeInboundSMSData(fragmentTestRequest("after-restart"), second, "SIP/2.0 200 OK\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	assertRecoveredSMS(t, sub, "first second")
	records, err := store.LoadInboundFragments(secondService.inboundFragmentOwner())
	if err != nil || len(records) != 0 {
		t.Fatalf("assembly left %d records: %v", len(records), err)
	}
}

func TestMixedSMSCenterFragmentsKeepRecipientAndReferenceIsolation(t *testing.T) {
	for _, field := range []string{"sender", "recipient", "reference", "reference-width"} {
		t.Run(field, func(t *testing.T) {
			service, sub := newFragmentPersistenceService(t, newMemoryInboundFragmentStore())
			first := persistedFragmentMessage(1, "first ")
			second := persistedFragmentMessage(2, "other")
			second.serviceCenter = "+12025550102"
			switch field {
			case "sender":
				second.sender = "Other"
			case "recipient":
				second.targetURI = "sip:+447000000000@ims.example"
			case "reference":
				second.concatRef++
			case "reference-width":
				second.refBits = 16
			}
			for _, part := range []inboundSMS{first, second} {
				if _, err := service.finalizeInboundSMSData(fragmentTestRequest(field), part, "SIP/2.0 200 OK\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
			}
			assertNoIMSEvent(t, sub, "different "+field)
		})
	}
}

func TestLegacySMSCenterFragmentGroupsStayIsolated(t *testing.T) {
	store := newMemoryInboundFragmentStore()
	service, sub := newFragmentPersistenceService(t, store)
	first := persistedFragmentMessage(1, "legacy ")
	if _, err := service.finalizeInboundSMSData(fragmentTestRequest("legacy-seed"), first, "SIP/2.0 200 OK\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	key := inboundSMSFragmentKey(first)
	oldScope := service.inboundFragmentScope("sender=giffgaff|ref=198|bits=8|sc=+447802000332|local=+447840844894")
	newScope := service.inboundFragmentScope(key)
	store.fragmentMu.Lock()
	fragments := store.fragments[newScope]
	delete(store.fragments, newScope)
	store.fragments[oldScope] = fragments
	store.fragmentMu.Unlock()
	service.StopCurrent()
	restored, restoredSub := newFragmentPersistenceService(t, store)
	if err := restored.restoreInboundFragments(); err != nil {
		t.Fatal(err)
	}
	assertNoIMSEvent(t, sub, "legacy seed")
	second := persistedFragmentMessage(2, "new")
	second.serviceCenter = "+12025550102"
	if _, err := restored.finalizeInboundSMSData(fragmentTestRequest("new-segment"), second, "SIP/2.0 200 OK\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	assertNoIMSEvent(t, restoredSub, "legacy group must not combine with new group")
	records, err := store.LoadInboundFragments(restored.inboundFragmentOwner())
	if err != nil || len(records) != 2 {
		t.Fatalf("legacy preservation: count=%d err=%v", len(records), err)
	}
}
