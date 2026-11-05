package imscore

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/iniwex5/vowifi-go/internal/vowifi/policy"
)

type fakeMWIPolicyStore struct {
	mode     string
	rejected int
	loadErr  error
	saved    []int
	identity string
	iccid    string
}

func (f *fakeMWIPolicyStore) LoadMWISubscriptionPolicy(iccid, identity string) (string, int, error) {
	return f.mode, f.rejected, f.loadErr
}

func (f *fakeMWIPolicyStore) SaveMWISubscriptionRejection(iccid, identity string, status int) error {
	f.saved, f.iccid, f.identity = append(f.saved, status), iccid, identity
	return nil
}

func newMWIPolicyTestService(t *testing.T, store *fakeMWIPolicyStore, carrierDisabled bool) *Service {
	t.Helper()
	s := newSubscriptionLifecycleTestService(t, nil)
	s.cfg.ICCID = "8949020000000000001"
	s.cfg.RegisterTemplate.DisableMWISubscription = carrierDisabled
	if store != nil {
		s.cfg.MWIPolicyStore = store
	}
	return s
}

func TestMWISubscriptionBlockedReason(t *testing.T) {
	cases := []struct {
		name            string
		store           *fakeMWIPolicyStore
		carrierDisabled bool
		blocked         bool
	}{
		{name: "no store follows carrier default", blocked: false},
		{name: "no store follows carrier opt-out", carrierDisabled: true, blocked: true},
		{name: "auto follows carrier opt-out", store: &fakeMWIPolicyStore{mode: MWISubscriptionAuto}, carrierDisabled: true, blocked: true},
		{name: "auto subscribes elsewhere", store: &fakeMWIPolicyStore{mode: MWISubscriptionAuto}, blocked: false},
		{name: "on overrides carrier opt-out", store: &fakeMWIPolicyStore{mode: MWISubscriptionOn}, carrierDisabled: true, blocked: false},
		{name: "off never subscribes", store: &fakeMWIPolicyStore{mode: MWISubscriptionOff}, blocked: true},
		{name: "remembered rejection blocks auto", store: &fakeMWIPolicyStore{mode: MWISubscriptionAuto, rejected: 405}, blocked: true},
		{name: "remembered rejection blocks on", store: &fakeMWIPolicyStore{mode: MWISubscriptionOn, rejected: 489}, blocked: true},
		{name: "unreadable policy does not subscribe", store: &fakeMWIPolicyStore{loadErr: errors.New("db closed")}, blocked: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newMWIPolicyTestService(t, tc.store, tc.carrierDisabled)
			if reason := s.mwiSubscriptionBlockedReason(); (reason != "") != tc.blocked {
				t.Fatalf("blocked reason = %q, want blocked=%v", reason, tc.blocked)
			}
		})
	}
}

func TestMWISubscriptionOffDoesNotStart(t *testing.T) {
	s := newMWIPolicyTestService(t, &fakeMWIPolicyStore{mode: MWISubscriptionOff}, false)
	s.startMWISubscription()
	s.mu.RLock()
	started := s.mwiSubscriptionLifecycle.started
	s.mu.RUnlock()
	if started || s.mwiSubscriptionInFlight.Load() {
		t.Fatal("SIM setting off still started SUBSCRIBE(mwi)")
	}
}

func TestBlockedMWIRefreshStopsTheUsage(t *testing.T) {
	store := &fakeMWIPolicyStore{mode: MWISubscriptionAuto}
	s := newMWIPolicyTestService(t, store, false)
	if start, reason := s.prepareSubscriptionStart(true); !start {
		t.Fatal(reason)
	}
	s.mu.Lock()
	s.mwiSubscriptionDialog = registrationSubscriptionDialog{callID: "call", localTag: "local", remoteTag: "remote"}
	s.mwiSubscriptionRefreshAt = time.Now()
	s.mu.Unlock()
	store.mode = MWISubscriptionOff
	s.refreshMWISubscription()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.mwiSubscriptionClosed || !s.mwiSubscriptionRefreshAt.IsZero() || s.mwiSubscriptionDialog.ready() {
		t.Fatal("blocked refresh left the MWI usage scheduled")
	}
	if s.mwiSubscriptionLifecycle.started || s.subscriptionDueLocked(true, time.Now().Add(time.Hour)) {
		t.Fatal("maintenance would pick the blocked MWI usage again")
	}
	if s.mwiSubscriptionInFlight.Load() {
		t.Fatal("blocked refresh still sent SUBSCRIBE(mwi)")
	}
}

func TestNetworkMWIRejectionIsRemembered(t *testing.T) {
	for _, status := range []int{405, 489} {
		store := &fakeMWIPolicyStore{mode: MWISubscriptionAuto}
		s := newMWIPolicyTestService(t, store, false)
		recordMWIStatusForTest(t, s, status, false)
		if len(store.saved) != 1 || store.saved[0] != status {
			t.Fatalf("status %d saved = %v", status, store.saved)
		}
		if store.iccid != s.cfg.ICCID || store.identity == "" {
			t.Fatalf("status %d saved for iccid %q identity %q", status, store.iccid, store.identity)
		}
	}
}

func TestTemporaryOrLocalMWIFailuresAreNotRemembered(t *testing.T) {
	for _, tc := range []struct {
		status     int
		selfRouted bool
	}{{480, false}, {408, false}, {500, false}, {403, false}, {405, true}} {
		store := &fakeMWIPolicyStore{mode: MWISubscriptionAuto}
		s := newMWIPolicyTestService(t, store, false)
		recordMWIStatusForTest(t, s, tc.status, tc.selfRouted)
		if len(store.saved) != 0 {
			t.Fatalf("status %d self-routed=%v was remembered", tc.status, tc.selfRouted)
		}
	}
}

func recordMWIStatusForTest(t *testing.T, s *Service, status int, selfRouted bool) {
	t.Helper()
	attempt, err := s.beginSubscriptionAttempt(true, false)
	if err != nil {
		t.Fatal(err)
	}
	if selfRouted {
		s.mu.Lock()
		s.mwiSubscriptionLifecycle.selfRouted = true
		s.mu.Unlock()
	}
	result := subscriptionResult{context: attempt, response: &sip.Response{StatusCode: status}, err: errors.New("subscription failed")}
	if err := s.recordMWISubscriptionResult(result); err == nil {
		t.Fatal("failure was hidden")
	}
}

type flippingMWIPolicyStore struct {
	loads atomic.Int32
}

// The first load allows MWI, every later load reports the setting turned off.
func (f *flippingMWIPolicyStore) LoadMWISubscriptionPolicy(string, string) (string, int, error) {
	if f.loads.Add(1) == 1 {
		return MWISubscriptionAuto, 0, nil
	}
	return MWISubscriptionOff, 0, nil
}

func (f *flippingMWIPolicyStore) SaveMWISubscriptionRejection(string, string, int) error { return nil }

func TestMWI481RetryRechecksPolicy(t *testing.T) {
	service := newProtectedKeepaliveTestService(t)
	service.cfg.MWIPolicyStore = &flippingMWIPolicyStore{}
	client, server := net.Pipe()
	service.activateProtectedRegistrationTCP(client)
	t.Cleanup(func() { _ = server.Close() })
	service.mwiSubscriptionDialog = registrationSubscriptionDialog{
		callID: "mwi-old", localTag: "local", remoteTag: "remote", cseq: 4,
	}
	requests := make(chan string, 2)
	go func() {
		reader := bufio.NewReader(server)
		for {
			request, err := readSIPStreamMessage(reader)
			if err != nil {
				return
			}
			requests <- request
			if _, err = io.WriteString(server, subscriptionWireResponse(request, 481, "")); err != nil {
				return
			}
		}
	}()
	if err := service.sendSubscribeMWI(context.Background()); err != nil {
		t.Fatalf("sendSubscribeMWI: %v", err)
	}
	_ = server.Close()
	if got := len(requests); got != 1 {
		t.Fatalf("SUBSCRIBE(mwi) sent %d times; the 481 retry ignored the policy", got)
	}
	service.mu.RLock()
	defer service.mu.RUnlock()
	if !service.mwiSubscriptionClosed || service.mwiSubscriptionLifecycle.started {
		t.Fatal("blocked 481 retry left the MWI usage running")
	}
}

func TestCarrierMWISwitchReachesTheLegacyTemplate(t *testing.T) {
	cfg := &IMSConfig{IMSRegisterTemplate: policy.IMSRegisterTemplate{ID: "vodafone_de_26202", DisableMWISubscription: true}}
	mergeOriginalRegisterTemplate(cfg)
	if !cfg.RegisterTemplate.DisableMWISubscription {
		t.Fatal("carrier MWI switch did not reach the register template")
	}
}

func TestRegistrationStopsAnActiveMWIUsageTurnedOff(t *testing.T) {
	store := &fakeMWIPolicyStore{mode: MWISubscriptionAuto}
	s := newMWIPolicyTestService(t, store, false)
	if start, reason := s.prepareSubscriptionStart(true); !start {
		t.Fatal(reason)
	}
	s.mu.Lock()
	s.mwiSubscriptionDialog = registrationSubscriptionDialog{callID: "call", localTag: "local", remoteTag: "remote"}
	s.mwiSubscriptionRefreshAt = time.Now().Add(time.Hour)
	s.mu.Unlock()
	store.mode = MWISubscriptionOff
	s.startMWISubscription()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.mwiSubscriptionLifecycle.started || !s.mwiSubscriptionClosed || !s.mwiSubscriptionRefreshAt.IsZero() {
		t.Fatal("registration kept an MWI usage the SIM setting turned off")
	}
}

// The check must run when the dispatch worker sends, not when the request is
// queued: a setting saved while the request waits in the queue still applies.
func TestMWIPolicyIsCheckedAfterTheDispatchQueue(t *testing.T) {
	service := newProtectedKeepaliveTestService(t)
	store := &fakeMWIPolicyStore{mode: MWISubscriptionAuto}
	service.cfg.MWIPolicyStore = store
	client, server := net.Pipe()
	service.activateProtectedRegistrationTCP(client)
	t.Cleanup(func() { _ = server.Close() })
	sent := make(chan string, 1)
	go func() {
		request, err := readSIPStreamMessage(bufio.NewReader(server))
		if err == nil {
			sent <- request
		}
	}()
	queue := make(chan outboundRequestTask, 1)
	service.outboundMu.Lock()
	service.outboundReqShards = []chan outboundRequestTask{queue}
	service.outboundMu.Unlock()

	result := make(chan error, 1)
	go func() { result <- service.sendSubscribeMWI(context.Background()) }()
	var task outboundRequestTask
	select {
	case task = <-queue:
	case <-time.After(2 * time.Second):
		t.Fatal("SUBSCRIBE(mwi) never reached the dispatch queue")
	}
	store.mode = MWISubscriptionOff
	service.runOutboundRequestTask(0, task)
	if err := <-result; err != nil {
		t.Fatalf("sendSubscribeMWI: %v", err)
	}
	select {
	case request := <-sent:
		t.Fatalf("queued SUBSCRIBE(mwi) was sent after the setting turned off:\n%s", request)
	case <-time.After(200 * time.Millisecond):
	}
	service.mu.RLock()
	defer service.mu.RUnlock()
	if service.mwiSubscriptionLifecycle.started || !service.mwiSubscriptionClosed {
		t.Fatal("queued block left the MWI usage running")
	}
}

func TestMWIPolicyBlockDoesNotKillSignaling(t *testing.T) {
	service := newProtectedKeepaliveTestService(t)
	client, server := net.Pipe()
	service.activateProtectedRegistrationTCP(client)
	t.Cleanup(func() { _ = server.Close() })
	blocked := &mwiPolicyBlockedError{reason: "MWI policy unavailable: dial tcp 127.0.0.1:5432: connect: connection refused"}
	service.handleOutboundRequestError(outboundModeContext{}, nil, fmt.Errorf("SUBSCRIBE transaction: %w", blocked))
	service.mu.RLock()
	defer service.mu.RUnlock()
	if service.registrationTCP == nil {
		t.Fatal("a policy block was treated as a dead signaling connection")
	}
}
