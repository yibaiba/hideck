package imscore

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

func TestREGISTERRefreshDoesNotRestartRejectedSubscriptions(t *testing.T) {
	s := newSubscriptionLifecycleTestService(t, nil)
	s.mu.Lock()
	s.regSession.callID = "subscription-refresh-register"
	s.externalTransport = true
	s.mu.Unlock()
	rejectSubscriptionForTest(t, s, false)
	rejectSubscriptionForTest(t, s, true)
	s.transport.SetSendFn(func(request string) error {
		s.transport.DeliverResponse(registerResponseForRequest(request, 200, map[string]string{"Expires": "3600"}))
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Register(ctx); err != nil {
		t.Fatal(err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.subscriptionClosed || !s.mwiSubscriptionClosed || s.subscriptionLastErr == "" || s.mwiSubscriptionLastErr == "" {
		t.Fatal("REGISTER cleared rejected subscriptions")
	}
}

func newSubscriptionLifecycleTestService(t *testing.T, store *SubscriptionRegistrationStore) *Service {
	return newSubscriptionLifecycleTestServiceForPreset(t, store, "")
}

func newSubscriptionLifecycleTestServiceForPreset(
	t *testing.T,
	store *SubscriptionRegistrationStore,
	preset string,
) *Service {
	t.Helper()
	s := newProtectedKeepaliveTestService(t)
	s.cfg.CarrierPresetID = preset
	client, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })
	s.activateProtectedRegistrationTCP(client)
	s.mu.Lock()
	if store != nil {
		s.subscriptionRegistrations = store
	}
	s.regSession.expires = time.Hour
	s.trackSubscriptionRegistrationLocked(time.Hour)
	s.mu.Unlock()
	return s
}

func rejectSubscriptionForTest(t *testing.T, s *Service, mwi bool) {
	t.Helper()
	attempt, err := s.beginSubscriptionAttempt(mwi, false)
	if err != nil {
		t.Fatal(err)
	}
	status := 489
	if mwi {
		status = 405
	}
	result := subscriptionResult{context: attempt, response: &sip.Response{StatusCode: status}, err: errors.New("subscription rejected")}
	if mwi {
		err = s.recordMWISubscriptionResult(result)
	} else {
		err = s.recordSubscriptionResult(result)
	}
	if err == nil {
		t.Fatal("rejection was hidden")
	}
}

func TestSubscriptionRejectionsSurviveOrdinaryRegisterRefresh(t *testing.T) {
	s := newSubscriptionLifecycleTestService(t, nil)
	rejectSubscriptionForTest(t, s, false)
	rejectSubscriptionForTest(t, s, true)
	for range 3 {
		s.mu.Lock()
		s.trackSubscriptionRegistrationLocked(time.Hour)
		s.mu.Unlock()
		for _, mwi := range []bool{false, true} {
			if start, _ := s.prepareSubscriptionStart(mwi); start {
				t.Fatalf("REGISTER refresh retried mwi=%t", mwi)
			}
		}
	}
	if s.subscriptionLastErr == "" || s.mwiSubscriptionLastErr == "" {
		t.Fatal("rejection diagnostics erased")
	}
}

func TestSubscriptionRejectionExpiryAllowsRetryWithoutServiceRestart(t *testing.T) {
	service := newSubscriptionLifecycleTestService(t, nil)
	rejectSubscriptionForTest(t, service, true)
	service.subscriptionRegistrations.mu.Lock()
	entry := service.subscriptionRegistrations.identities[service.subscriptionBinding.identity]
	entry.rejections[mwiEventPackage] = subscriptionRejection{
		status: 405, expiresAt: time.Now().Add(-time.Second),
	}
	service.subscriptionRegistrations.mu.Unlock()
	service.mu.Lock()
	due := service.subscriptionDueLocked(true, time.Now())
	service.mu.Unlock()
	if !due {
		t.Fatal("expired rejection did not schedule an automatic MWI retry")
	}
	if start, reason := service.prepareSubscriptionStart(true); !start {
		t.Fatalf("expired rejection still blocked MWI: %s", reason)
	}
}

func TestCarrierPolicySkipsMWISubscription(t *testing.T) {
	s := newSubscriptionLifecycleTestService(t, nil)
	s.cfg.RegisterTemplate.DisableMWISubscription = true
	s.startMWISubscription()
	s.mu.RLock()
	started := s.mwiSubscriptionLifecycle.started
	s.mu.RUnlock()
	if started || s.mwiSubscriptionInFlight.Load() {
		t.Fatal("carrier policy did not stop SUBSCRIBE(mwi)")
	}
	if start, reason := s.prepareSubscriptionStart(false); !start {
		t.Fatalf("carrier MWI policy blocked registration event subscription: %s", reason)
	}
}

func TestRegisteredSubscriptionDialogSurvivesRegisterRefresh(t *testing.T) {
	s := newSubscriptionLifecycleTestService(t, nil)
	for _, mwi := range []bool{false, true} {
		if start, reason := s.prepareSubscriptionStart(mwi); !start {
			t.Fatal(reason)
		}
	}
	dialog := registrationSubscriptionDialog{callID: "call", localTag: "local", remoteTag: "remote"}
	refresh := time.Now().Add(time.Minute)
	s.mu.Lock()
	s.subscriptionDialog, s.mwiSubscriptionDialog = dialog, dialog
	s.subscriptionRefreshAt, s.mwiSubscriptionRefreshAt = refresh, refresh
	s.trackSubscriptionRegistrationLocked(time.Hour)
	s.mu.Unlock()
	s.startRegistrationSubscription()
	s.startMWISubscription()
	if s.subscriptionDialog.callID != "call" || s.mwiSubscriptionDialog.callID != "call" ||
		!s.subscriptionRefreshAt.Equal(refresh) || !s.mwiSubscriptionRefreshAt.Equal(refresh) {
		t.Fatal("REGISTER refresh replaced live subscriptions")
	}
}

func TestSubscriptionNewContactPreservesExplicitRejections(t *testing.T) {
	store := NewSubscriptionRegistrationStore()
	s := newSubscriptionLifecycleTestService(t, store)
	rejectSubscriptionForTest(t, s, false)
	rejectSubscriptionForTest(t, s, true)
	s.mu.Lock()
	s.regSession.contactUser = "new-contact"
	s.subscriptionGeneration++
	s.trackSubscriptionRegistrationLocked(time.Hour)
	s.mu.Unlock()
	if start, _ := s.prepareSubscriptionStart(false); start {
		t.Fatal("new Contact cleared registration event rejection")
	}
	if start, _ := s.prepareSubscriptionStart(true); start {
		t.Fatal("new Contact cleared MWI rejection")
	}
	replacement := newSubscriptionLifecycleTestService(t, store)
	for _, mwi := range []bool{false, true} {
		if start, _ := replacement.prepareSubscriptionStart(mwi); start {
			t.Fatalf("Service rebuild cleared rejection for mwi=%t", mwi)
		}
	}
}

func TestSubscriptionLateResultsDoNotOverwriteNewContext(t *testing.T) {
	for _, mwi := range []bool{false, true} {
		for _, status := range []int{200, 405, 489} {
			s := newSubscriptionLifecycleTestService(t, nil)
			old, err := s.beginSubscriptionAttempt(mwi, false)
			if err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			s.subscriptionGeneration++
			s.mu.Unlock()
			result := subscriptionResult{context: old, response: &sip.Response{StatusCode: status}, requestedExpires: time.Hour}
			if status != 200 {
				result.err = errors.New("old rejection")
			}
			if mwi {
				err = s.recordMWISubscriptionResult(result)
			} else {
				err = s.recordSubscriptionResult(result)
			}
			if !errors.Is(err, errSubscriptionContextChanged) {
				t.Fatalf("late response accepted: %v", err)
			}
			if s.subscriptionLastErr != "" || s.mwiSubscriptionLastErr != "" || storeMWIRejection(s) != 0 {
				t.Fatal("old response changed new status")
			}
		}
	}
}

func TestSubscriptionResponseDuringRegisterRefreshRemainsValid(t *testing.T) {
	s := newSubscriptionLifecycleTestService(t, nil)
	for _, mwi := range []bool{false, true} {
		attempt, err := s.beginSubscriptionAttempt(mwi, false)
		if err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		s.regState = regRegistering
		s.mu.Unlock()
		result := subscriptionResult{context: attempt, response: &sip.Response{StatusCode: 200}, requestedExpires: time.Hour}
		if mwi {
			err = s.recordMWISubscriptionResult(result)
		} else {
			err = s.recordSubscriptionResult(result)
		}
		if err != nil {
			t.Fatalf("REGISTER refresh discarded current subscription response: %v", err)
		}
	}
}

func TestSubscriptionDeregistrationAllowsNewMWIAttempt(t *testing.T) {
	s := newSubscriptionLifecycleTestService(t, nil)
	rejectSubscriptionForTest(t, s, true)
	s.mu.Lock()
	s.endSubscriptionRegistrationLocked(false)
	s.trackSubscriptionRegistrationLocked(time.Hour)
	s.mu.Unlock()
	if start, reason := s.prepareSubscriptionStart(true); !start {
		t.Fatalf("new registration skipped MWI: %s", reason)
	}
}

func storeMWIRejection(s *Service) int {
	return s.subscriptionRegistrations.rejected(s.subscriptionBinding, mwiEventPackage)
}

func Test2degreesMWIRejectionDoesNotChangeOnDemandSMSReadiness(t *testing.T) {
	s := newSubscriptionLifecycleTestServiceForPreset(t, nil, "2degrees_nz_53024")
	rejectSubscriptionForTest(t, s, true)
	push, peer := net.Pipe()
	t.Cleanup(func() { _ = push.Close(); _ = peer.Close() })
	s.trackProtectedConnection(push)
	s.recordPortSOpened(push, time.Now())
	s.portSOnDemandObserved.Store(true)
	s.recordPortSClosed(push, io.EOF, time.Now())
	s.untrackProtectedConnection(push)
	if got := s.SMSReadiness(); !got.Ready {
		t.Fatalf("MWI rejection changed on-demand readiness: %+v", got)
	}
	if start, _ := s.prepareSubscriptionStart(true); start {
		t.Fatal("clean EOF restarted MWI subscription")
	}
	s.trackProtectedConnection(push)
	if got := s.SMSReadiness(); !got.Ready {
		t.Fatalf("downlink reconnect did not restore readiness: %+v", got)
	}
	select {
	case err := <-s.RegistrationErrors():
		t.Fatalf("subscription rejection requested IMS rebuild: %v", err)
	default:
	}
}

func TestSelfRoutedMWI405IsNotPersistedAsNetworkCapability(t *testing.T) {
	persistence := &memorySubscriptionRejectionStore{
		values: make(map[string]persistedSubscriptionRejection),
	}
	registrations := NewPersistentSubscriptionRegistrationStore(persistence)
	service := newSubscriptionLifecycleTestService(t, registrations)
	attempt, err := service.beginSubscriptionAttempt(true, false)
	if err != nil {
		t.Fatal(err)
	}
	request, requestedExpires, err := service.buildMWISubscription(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	result := subscriptionResult{
		context: attempt, request: request, requestedExpires: requestedExpires,
	}
	if err := service.recordMWISubscriptionAttempt(result); err != nil {
		t.Fatal(err)
	}
	service.markSelfRoutedSubscription(request)
	result.response = &sip.Response{StatusCode: 405, Reason: "Method Not Allowed"}
	result.err = errors.New("SUBSCRIBE rejected with status 405")
	if err := service.recordMWISubscriptionResult(result); err == nil {
		t.Fatal("self-routed rejection was hidden")
	}
	if got := registrations.rejected(service.subscriptionBinding, mwiEventPackage); got != 0 {
		t.Fatalf("self-routed 405 became network rejection: %d", got)
	}
	if len(persistence.values) != 0 {
		t.Fatalf("self-routed 405 was persisted: %#v", persistence.values)
	}
	if start, reason := service.prepareSubscriptionStart(true); start || !strings.Contains(reason, "routed back") {
		t.Fatalf("current context retried self-routed MWI: start=%t reason=%q", start, reason)
	}
	service.mu.Lock()
	service.subscriptionGeneration++
	service.trackSubscriptionRegistrationLocked(time.Hour)
	service.mu.Unlock()
	if start, reason := service.prepareSubscriptionStart(true); !start {
		t.Fatalf("new registration context retained self-route result: %s", reason)
	}
}
