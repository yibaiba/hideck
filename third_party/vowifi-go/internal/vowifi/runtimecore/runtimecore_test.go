package runtimecore

import (
	"bytes"
	"context"
	"errors"
	"github.com/iniwex5/vowifi-go/runtimehost/carrier"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	enginesim "github.com/iniwex5/vowifi-go/engine/sim"
	"github.com/iniwex5/vowifi-go/engine/swu"
	"github.com/iniwex5/vowifi-go/internal/vowifi/imscore"
	"github.com/iniwex5/vowifi-go/internal/vowifi/imsendpoint"
	"github.com/iniwex5/vowifi-go/internal/vowifi/ipsec3gpp"
	"github.com/iniwex5/vowifi-go/internal/vowifi/netstack"
	"github.com/iniwex5/vowifi-go/internal/vowifi/policy"
	"github.com/iniwex5/vowifi-go/internal/vowifi/profile"
	"github.com/iniwex5/vowifi-go/internal/vowifi/simauth"
	"github.com/iniwex5/vowifi-go/internal/vowifi/smsdelivery"
)

var (
	_ error                                                                                               = ErrRedirect{}
	_ func(context.Context, RuntimeStartRequest) (RuntimeStartResult, error)                              = (Runtime{}).Start
	_ func(context.Context, SessionConfig) (*SessionResult, error)                                        = RunSession
	_ func(SessionConfig) *swu.Config                                                                     = BuildSWUConfig
	_ func(context.Context, func(int) int64, func(), func(int, int64), func(context.Context) error) error = RunLoop
)

type recordingAKAProvider struct {
	mu    sync.Mutex
	calls int
}

func (provider *recordingAKAProvider) CalculateAKA(rand16, autn16 []byte) (enginesim.AKAResult, error) {
	provider.mu.Lock()
	provider.calls++
	provider.mu.Unlock()
	return enginesim.AKAResult{
		RES: append([]byte(nil), rand16...), CK: append([]byte(nil), autn16...),
	}, nil
}

type testSIMAdapter struct {
	aka      enginesim.AKAProvider
	identity profile.Provider
}

func (adapter testSIMAdapter) EPDGSIMProvider(profile.AuthPlan) enginesim.AKAProvider {
	return adapter.aka
}

func (adapter testSIMAdapter) IMSAKAProvider(profile.AuthPlan) simauth.AKAProvider {
	return adapter.aka
}

func (adapter testSIMAdapter) IMSIdentityProvider() profile.Provider { return adapter.identity }

type failingIdentityProvider struct{ err error }

func (provider failingIdentityProvider) GetISIMIdentity() (profile.Identity, error) {
	return profile.Identity{}, provider.err
}

func TestPrepareSessionStartResolvesCarrierAndOverride(t *testing.T) {
	provider := &recordingAKAProvider{}
	prepared, err := PrepareSessionStart(context.Background(), RuntimeStartRequest{
		Profile:             profile.Profile{IMSI: "234102356143376", MCC: "234", MNC: "10"},
		RuntimeEPDGOverride: "epdg.override.example",
		IMSIdentity: profile.IMSIdentityResult{
			IMPI: "234102356143376@ims.example", IMPU: "sip:user@ims.example", Domain: "ims.example",
		},
		SIM: testSIMAdapter{aka: provider},
	})
	if err != nil {
		t.Fatalf("PrepareSessionStart() error = %v", err)
	}
	if prepared.EPDGAddr != "epdg.override.example" || prepared.EPDGSource != "redirect" {
		t.Fatalf("runtime override not applied: %+v", prepared)
	}
	if prepared.CarrierPlan.Metadata.PresetID != "giffgaff_23410" {
		t.Fatalf("carrier preset = %q", prepared.CarrierPlan.Metadata.PresetID)
	}
	if prepared.Profile.IMEI == "" || prepared.IdentityIMEISource == "" {
		t.Fatalf("device identity was not resolved: %+v", prepared)
	}
}

func TestPrepareSessionStartPropagatesIdentityFailure(t *testing.T) {
	want := errors.New("reader transport failed")
	_, err := PrepareSessionStart(context.Background(), RuntimeStartRequest{
		Profile: profile.Profile{IMSI: "310280233621715", MCC: "310", MNC: "280"},
		SIM: testSIMAdapter{
			aka: &recordingAKAProvider{}, identity: failingIdentityProvider{err: want},
		},
	})
	if !errors.Is(err, want) {
		t.Fatalf("PrepareSessionStart() error = %v, want %v", err, want)
	}
}

func TestBuildIMSConfigDerivesUnappliedIdentity(t *testing.T) {
	prepared := profile.PreparedSession{
		Profile: profile.Profile{IMSI: "234102356143376", IMSDomain: "ims.mnc010.mcc234.3gppnetwork.org"},
		CarrierPlan: policy.CarrierPlan{IMS: policy.IMSPlan{
			Domain: "ims.mnc010.mcc234.3gppnetwork.org",
		}},
	}
	config, err := buildIMSConfig(imsConfigInput{
		session: SessionConfig{Prepared: prepared}, result: &SessionResult{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.IMPI != "234102356143376@ims.mnc010.mcc234.3gppnetwork.org" ||
		config.IMPU != "sip:"+config.IMPI {
		t.Fatalf("derived IMS config identity = %q %+v", config.IMPI, config.IMPU)
	}
}

func TestBuildIMSConfigPreservesDisabledSecAgreePolicy(t *testing.T) {
	prepared := profile.PreparedSession{
		Profile: profile.Profile{IMSI: "234102356143376", MCC: "234", MNC: "10"},
		CarrierPlan: policy.CarrierPlan{IMS: policy.IMSPlan{
			RegisterTemplate: policy.IMSRegisterTemplate{SecAgreeMode: "disabled"},
		}},
	}
	config, err := buildIMSConfig(imsConfigInput{
		session: SessionConfig{Prepared: prepared}, result: &SessionResult{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.IPSec3GPPEnabled() {
		t.Fatal("buildIMSConfig enabled 3GPP IPsec despite disabled carrier policy")
	}
}

func TestPreparedSessionOverrideIsDetached(t *testing.T) {
	original := profile.PreparedSession{
		Profile:  profile.Profile{IMSI: "234102356143376"},
		EPDGAddr: "old.example",
		CarrierPlan: policy.CarrierPlan{IKE: policy.IKEPlan{
			IKEProposals: []string{"aes128-sha256-modp2048"},
		}},
	}
	prepared, err := PrepareSessionStart(context.Background(), RuntimeStartRequest{
		Prepared: &original, RuntimeEPDGOverride: "new.example",
	})
	if err != nil {
		t.Fatalf("PrepareSessionStart() error = %v", err)
	}
	prepared.CarrierPlan.IKE.IKEProposals[0] = "changed"
	if original.EPDGAddr != "old.example" || original.CarrierPlan.IKE.IKEProposals[0] == "changed" {
		t.Fatal("PrepareSessionStart() mutated caller-owned prepared session")
	}
}

func TestCompatibilityPreparedSessionPreservesIdentityContext(t *testing.T) {
	input := &PreparedSessionStart{
		Profile:  profile.Profile{IMSI: "234102356143376"},
		EPDGAddr: "epdg.example.com",
		IMSIdentityResult: profile.IMSIdentityResult{
			RequestedSource: "isim", ActualSource: "usim", AKAAppPreference: "auto",
			Applied: true, IMPI: "user@ims.example", IMPU: "sip:user@ims.example",
			Domain: "ims.example",
		},
		IdentityIMEISource: "carrier_model",
	}
	prepared, err := AdaptCompatibilityPreparedSession(input)
	if err != nil {
		t.Fatalf("AdaptCompatibilityPreparedSession: %v", err)
	}
	if !reflect.DeepEqual(prepared.IMSIdentity, input.IMSIdentityResult) ||
		prepared.IdentityIMEISource != input.IdentityIMEISource {
		t.Fatalf("prepared identity context = %+v source=%q", prepared.IMSIdentity, prepared.IdentityIMEISource)
	}
}

func TestBuildSWUConfigCarriesRuntimeState(t *testing.T) {
	provider := &recordingAKAProvider{}
	prepared := profile.PreparedSession{
		Profile:  profile.Profile{IMSI: "234102356143376", MCC: "234", MNC: "10", IMEI: "123456789012345"},
		EPDGAddr: "epdg.example.com",
		AuthPlan: profile.NewAuthPlan(profile.AKAAppUSIM, profile.AKAAppUSIM),
		CarrierPlan: policy.CarrierPlan{
			EPDG: policy.EPDGPlan{Addr: "epdg.example.com", Port: 4500, APN: "ims", DNSServer: "1.1.1.1"},
			IKE: policy.IKEPlan{
				IKEProposals: []string{"aes128-sha256-modp2048"}, ESPProposals: []string{"aes128-sha256"},
				NATKeepaliveSeconds: 20, DPDIntervalSeconds: 120, ReauthIntervalSeconds: 180,
				IKERekeyIntervalSeconds: 9000,
			},
		},
	}
	ticket := []byte{1, 2}
	candidates := swu.NewEPDGCandidateStore(nil)
	config := BuildSWUConfig(SessionConfig{
		DeviceID: "dev-1", Prepared: prepared, SIM: testSIMAdapter{aka: provider},
		DataplaneMode: swu.DataplaneModeUserspace,
		ResumeTicket:  ticket, FastReauthID: "reauth@example", FastReauthMK: []byte{3, 4},
		EPDGCandidates: candidates,
	})
	if config.EPDGCandidates != candidates {
		t.Fatal("BuildSWUConfig dropped the runtime-owned ePDG candidate store")
	}
	if config.AKAProvider != provider || config.EPDGAddr != "epdg.example.com" || config.EpDGPort != 4500 {
		t.Fatalf("SWu config missing production fields: %+v", config)
	}
	if config.ReauthSeconds != 180*time.Second || config.RekeyIKESeconds != 9000*time.Second ||
		config.DataplaneMode != swu.DataplaneModeUserspace {
		t.Fatalf("SWu timers/dataplane = %+v", config)
	}
	if config.FastReauthID != "reauth@example" || !bytes.Equal(config.FastReauthMK, []byte{3, 4}) {
		t.Fatalf("FastReauth material = %q %v", config.FastReauthID, config.FastReauthMK)
	}
	if BuildSWUConfig(SessionConfig{Prepared: prepared, OmitInitialContact: true}).OmitInitialContact != true {
		t.Fatal("BuildSWUConfig() dropped OmitInitialContact")
	}
	prepared.CarrierPlan.IKE.AKAPrimePreferred = true
	if !BuildSWUConfig(SessionConfig{Prepared: prepared, SIM: testSIMAdapter{aka: provider}}).AKAPrimePreferred {
		t.Fatal("BuildSWUConfig() dropped AKAPrimePreferred")
	}
	prepared.CarrierPlan.IKE.IKEProposals[0] = "changed"
	if config.IKEProposals[0] == "changed" {
		t.Fatal("BuildSWUConfig() retained proposal slice alias")
	}
	ticket[0] = 9
	if !bytes.Equal(config.ResumeTicket, []byte{1, 2}) {
		t.Fatal("BuildSWUConfig() retained resume ticket alias")
	}
}

func TestBuildSWUConfigExposesMissingAKA(t *testing.T) {
	config := BuildSWUConfig(SessionConfig{Prepared: profile.PreparedSession{}})
	if _, err := config.AKAProvider.CalculateAKA(nil, nil); err == nil {
		t.Fatal("missing AKA provider reported success")
	}
}

func TestBuildIMSConfigUsesNegotiatedPCSCFAndCarrierRuntimeFields(t *testing.T) {
	penalties := imscore.NewRegistrarPenaltyStore()
	registrations := imscore.NewSubscriptionRegistrationStore()
	prepared := profile.PreparedSession{
		Profile: profile.Profile{
			IMSI: "234102356143376", MCC: "234", MNC: "10", IMEI: "123456789012345",
		},
		IMSIdentity: profile.IMSIdentityResult{
			IMPI: "234102356143376@ims.example", IMPU: "sip:user@ims.example",
			Domain: "ims.example",
		},
		CarrierPlan: policy.CarrierPlan{IMS: policy.IMSPlan{
			PCSCF: "carrier-pcscf.example:5070", OptionsPingIntervalSeconds: 37,
		}},
	}
	result := &SessionResult{
		LocalAddr: "10.0.0.2",
		Snapshot: swu.SessionSnapshot{
			PCSCFv4: []net.IP{
				net.ParseIP("192.0.2.10"), nil, net.ParseIP("192.0.2.11"),
				net.ParseIP("192.0.2.10"),
			},
			PCSCFv6: []net.IP{net.ParseIP("2001:db8::10")},
		},
	}
	config, err := buildIMSConfig(imsConfigInput{
		session: SessionConfig{
			Prepared: prepared, RegistrarPenalties: penalties, SubscriptionRegistrations: registrations,
			MWIPolicyStore: mwiPolicyStoreStub{},
		}, result: result,
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.MWIPolicyStore != (mwiPolicyStoreStub{}) {
		t.Fatal("IMS config dropped the MWI policy store")
	}
	if config.Registrar != "192.0.2.10:5060;192.0.2.11:5060" {
		t.Fatalf("registrar = %q", config.Registrar)
	}
	if config.KeepaliveInterval != 37*time.Second {
		t.Fatalf("keepalive = %s", config.KeepaliveInterval)
	}
	if config.CellularNetworkInfo != "" || config.PAccessNetworkCountry != "GB" {
		t.Fatalf("network identity = %q country=%q", config.CellularNetworkInfo, config.PAccessNetworkCountry)
	}
	if config.RegistrarPenalties != penalties {
		t.Fatal("IMS config did not retain the runtime P-CSCF penalty store")
	}
	if config.SubscriptionRegistrations != registrations {
		t.Fatal("IMS config replaced subscription registration state")
	}
	result.Snapshot.PCSCFv4 = []net.IP{net.ParseIP("192.0.2.20")}
	replacement, err := buildIMSConfig(imsConfigInput{
		session: SessionConfig{Prepared: prepared, RegistrarPenalties: penalties, SubscriptionRegistrations: registrations}, result: result,
	})
	if err != nil || replacement.Registrar != "192.0.2.20:5060" || replacement.RegistrarPenalties != penalties {
		t.Fatalf("replacement must use new tunnel candidates and keep recovery history: config=%+v err=%v", replacement, err)
	}
	if replacement.SubscriptionRegistrations != registrations {
		t.Fatal("replacement tunnel lost subscription registration state")
	}
}

func TestEmptyDataplaneModeSelectsUserspaceNetwork(t *testing.T) {
	_, err := resolveIMSNetwork(context.Background(), "", &SessionResult{})
	if err == nil || !strings.Contains(err.Error(), "inner packet endpoint") {
		t.Fatalf("resolveIMSNetwork(empty) error = %v", err)
	}
}

func TestDeviceIdentityRetryMatchesRecoveredErrors(t *testing.T) {
	config := &swu.Config{EnableDeviceIdentitySpoof: true}
	for _, message := range []string{"peer auth failed", "EAP \u8ba4\u8bc1\u5931\u8d25"} {
		if !shouldRetryDeviceIdentity(config, errors.New(message)) {
			t.Fatalf("shouldRetryDeviceIdentity(%q) = false", message)
		}
	}
	if shouldRetryDeviceIdentity(config, errors.New("DEVICE_IDENTITY rejected")) {
		t.Fatal("unrecovered error text triggered identity retry")
	}
}

func TestRunLoopRetriesAndReportsDelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls, before int
	var retries []int
	err := RunLoop(
		ctx,
		func(attempt int) int64 {
			if attempt != 0 {
				t.Fatalf("first retry attempt = %d", attempt)
			}
			return int64(time.Nanosecond)
		},
		func() { before++ },
		func(attempt int, _ int64) { retries = append(retries, attempt) },
		func(context.Context) error {
			calls++
			if calls == 1 {
				return errors.New("temporary failure")
			}
			cancel()
			return nil
		},
	)
	if !errors.Is(err, context.Canceled) || calls != 2 || before != 2 || !reflect.DeepEqual(retries, []int{1}) {
		t.Fatalf("RunLoop() err=%v calls=%d before=%d retries=%v", err, calls, before, retries)
	}
}

func TestApplyRedirectOverrideUsesNewAddressAndCapsHops(t *testing.T) {
	req := &RuntimeStartRequest{}
	if err := applyRedirectOverride(req, &ErrRedirect{NewEPDG: "epdg-a.example"}); err == nil {
		t.Fatal("first redirect should stay retryable")
	}
	if req.RuntimeEPDGOverride != "epdg-a.example" || req.redirectHops != 1 {
		t.Fatalf("first override = %+v hops=%d", req, req.redirectHops)
	}
	if err := applyRedirectOverride(req, &ErrRedirect{NewEPDG: "epdg-b.example"}); err == nil {
		t.Fatal("second redirect should stay retryable")
	}
	if req.RuntimeEPDGOverride != "epdg-b.example" || req.redirectHops != 2 {
		t.Fatalf("second override = %q hops=%d", req.RuntimeEPDGOverride, req.redirectHops)
	}
	if err := applyRedirectOverride(req, &ErrRedirect{NewEPDG: "epdg-a.example"}); !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("redirect loop err = %v", err)
	}
	req = &RuntimeStartRequest{}
	for i := 0; i < maxSWuRedirects; i++ {
		target := "epdg-" + string(rune('a'+i)) + ".example"
		if err := applyRedirectOverride(req, &ErrRedirect{NewEPDG: target}); errors.Is(err, ErrTooManyRedirects) {
			t.Fatalf("hop %d stopped early: %v", i+1, err)
		}
	}
	if err := applyRedirectOverride(req, &ErrRedirect{NewEPDG: "epdg-z.example"}); !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("hop limit err = %v", err)
	}
}

func TestRunLoopStopsAfterTooManyRedirects(t *testing.T) {
	err := RunLoop(context.Background(), nil, nil, nil, func(context.Context) error {
		return ErrTooManyRedirects
	})
	if !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("RunLoop() = %v", err)
	}
}

func TestRuntimeStartEmitsTerminalErrorAfterReconnectLoopStops(t *testing.T) {
	recorder := &eventRecorder{}
	req := baseRuntimeRequest(recorder)
	req.Reconnect = true
	req.SessionStarter = func(context.Context, SessionConfig) (*SessionResult, error) {
		return nil, ErrTooManyRedirects
	}

	_, err := (Runtime{}).Start(context.Background(), req)
	if !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("Runtime.Start() error = %v", err)
	}
	want := []string{"prepared", "connecting", "error", "terminal_error"}
	if got := recorder.kinds(); !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime events = %v, want %v", got, want)
	}
}

func TestRuntimeStartKeepsScheduledRegistrarRetryInCurrentLoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	recorder := &eventRecorder{}
	req := baseRuntimeRequest(recorder)
	req.Reconnect = true
	req.MWIPolicyStore = mwiPolicyStoreStub{}
	var firstStore *imscore.RegistrarPenaltyStore
	var subscriptions *imscore.SubscriptionRegistrationStore
	var candidates *swu.EPDGCandidateStore
	attempts := 0
	req.SessionStarter = func(_ context.Context, config SessionConfig) (*SessionResult, error) {
		attempts++
		if attempts == 1 {
			firstStore = config.RegistrarPenalties
			subscriptions = config.SubscriptionRegistrations
			candidates = config.EPDGCandidates
			return nil, scheduledRetryTestError{retryAt: time.Now().Add(10 * time.Millisecond)}
		}
		if config.RegistrarPenalties != firstStore {
			t.Fatal("runtime reconnect replaced the registrar penalty store")
		}
		if subscriptions == nil || config.SubscriptionRegistrations != subscriptions {
			t.Fatal("runtime reconnect replaced subscription registration state")
		}
		if candidates == nil || config.EPDGCandidates != candidates {
			t.Fatal("runtime reconnect replaced the ePDG candidate store")
		}
		if config.MWIPolicyStore != (mwiPolicyStoreStub{}) {
			t.Fatal("runtime reconnect dropped the MWI policy store")
		}
		cancel()
		return nil, context.Canceled
	}

	_, err := (Runtime{}).Start(ctx, req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Runtime.Start() error = %v", err)
	}
	if attempts != 2 || firstStore == nil {
		t.Fatalf("runtime attempts = %d, registrar store = %p", attempts, firstStore)
	}
	want := []string{"prepared", "connecting", "error", "retry", "prepared", "connecting", "error"}
	if got := recorder.kinds(); !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime events = %v, want %v", got, want)
	}
}

func TestRetryDecisionResetsRedirectAndFreshRuntime(t *testing.T) {
	delay, attempt := retryDecision(&ErrRedirect{Delay: 17}, 4, func(int) int64 { return 99 })
	if delay != 17 || attempt != 0 {
		t.Fatalf("redirect decision = (%d,%d)", delay, attempt)
	}
	delay, attempt = retryDecision(swu.ErrFreshRuntimeRequired, 4, nil)
	if delay != 0 || attempt != 0 {
		t.Fatalf("fresh-runtime decision = (%d,%d)", delay, attempt)
	}
}

type scheduledRetryTestError struct {
	retryAt time.Time
}

func (err scheduledRetryTestError) Error() string      { return "scheduled retry" }
func (err scheduledRetryTestError) RetryAt() time.Time { return err.retryAt }

func TestRetryDelayUntilUsesWrappedRegistrarDeadline(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	scheduled := scheduledRetryTestError{retryAt: now.Add(12 * time.Minute)}
	err := errors.Join(errors.New("IMS startup failed"), scheduled)

	delay, ok := retryDelayUntil(err, now)
	if !ok || time.Duration(delay) != 12*time.Minute {
		t.Fatalf("scheduled retry delay = %s, found=%t", time.Duration(delay), ok)
	}
}

func TestInterruptionCallbacksCarryRecoveredKindsAndDelay(t *testing.T) {
	session := swu.NewSession(&swu.Config{})
	outcomes := make(chan InterruptOutcome, 3)
	req := &RuntimeStartRequest{}
	chainSessionCallbacks(sessionCallbackConfig{
		ctx: context.Background(), request: req, session: session, outcomes: outcomes,
	})
	session.OnRedirect(" epdg.redirect.example ")
	redirect := <-outcomes
	if redirect.Kind != "redirect" || redirect.Reason != "redirect" ||
		redirect.RedirectEPDG != "epdg.redirect.example" || redirect.RetryDelay != int64(2*time.Second) {
		t.Fatalf("redirect outcome = %+v", redirect)
	}
	session.OnReauthNeeded()
	if reauth := <-outcomes; reauth.Kind != "reauth" || reauth.Reason != swu.ErrFreshRuntimeRequired.Error() {
		t.Fatalf("reauth outcome = %+v", reauth)
	}
	session.OnSessionDown()
	if down := <-outcomes; down.Kind != "session_down" || down.Reason != "swu_session_down" {
		t.Fatalf("session-down outcome = %+v", down)
	}
}

func TestWaitRuntimeInterruptionUsesRecoveredContextKind(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	outcome := waitRuntimeInterruption(ctx, &RuntimeStartRequest{}, nil)
	if outcome.Kind != "context_cancelled" || !strings.Contains(outcome.Reason, "canceled") {
		t.Fatalf("context outcome = %+v", outcome)
	}
}

type eventRecorder struct {
	mu     sync.Mutex
	events []RuntimeEvent[*SessionResult]
}

func (recorder *eventRecorder) OnRuntimeEvent(_ context.Context, event RuntimeEvent[*SessionResult]) {
	recorder.mu.Lock()
	recorder.events = append(recorder.events, event)
	recorder.mu.Unlock()
}

func (recorder *eventRecorder) kinds() []string {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	result := make([]string, 0, len(recorder.events))
	for _, event := range recorder.events {
		result = append(result, event.Kind)
	}
	return result
}

func baseRuntimeRequest(recorder *eventRecorder) RuntimeStartRequest {
	return RuntimeStartRequest{
		DeviceID: "dev-1",
		Prepared: &profile.PreparedSession{
			Profile: profile.Profile{IMSI: "234102356143376"}, EPDGAddr: "epdg.example.com",
		},
		SIM:        testSIMAdapter{aka: &recordingAKAProvider{}},
		Observer:   recorder,
		fastReauth: &FastReauthStore{},
	}
}

func TestRuntimeStartEmitsEstablishedOnlyForEstablishedSnapshot(t *testing.T) {
	recorder := &eventRecorder{}
	req := baseRuntimeRequest(recorder)
	req.BeforeSessionStart = func(_ context.Context, config SessionConfig) error {
		if got := recorder.kinds(); !reflect.DeepEqual(got, []string{"prepared"}) {
			t.Fatalf("events before session preflight = %v", got)
		}
		if config.TraceID != defaultRuntimeTraceID || config.DataplaneMode != swu.DataplaneModeUserspace {
			t.Fatalf("session defaults = trace %q mode %q", config.TraceID, config.DataplaneMode)
		}
		return nil
	}
	req.SessionStarter = func(_ context.Context, config SessionConfig) (*SessionResult, error) {
		session := &SessionResult{DeviceID: "dev-1", Snapshot: swu.SessionSnapshot{
			Established: true, IPv4: []byte{10, 0, 0, 1},
		}}
		config.OnTunnelReady(session)
		if got := recorder.kinds(); !reflect.DeepEqual(got, []string{"prepared", "connecting", "established"}) {
			t.Fatalf("events before IMS startup = %v", got)
		}
		return session, nil
	}
	result, err := (Runtime{}).Start(context.Background(), req)
	if err != nil || result.Session == nil || result.TraceID != defaultRuntimeTraceID {
		t.Fatalf("Runtime.Start() result=%+v error=%v", result, err)
	}
	if got := recorder.kinds(); !reflect.DeepEqual(got, []string{"prepared", "connecting", "established"}) {
		t.Fatalf("runtime events = %v", got)
	}
}

func TestRuntimeDryRunDoesNotEmitFakeEstablished(t *testing.T) {
	recorder := &eventRecorder{}
	req := baseRuntimeRequest(recorder)
	req.DryRun = true
	result, err := (Runtime{}).Start(context.Background(), req)
	if err != nil || result.Session != nil {
		t.Fatalf("Runtime.Start(dry-run) result=%+v error=%v", result, err)
	}
	if got := recorder.kinds(); len(got) != 0 {
		t.Fatalf("dry-run events = %v", got)
	}
}

func TestRuntimeStartDoesNotPromoteUnestablishedSession(t *testing.T) {
	recorder := &eventRecorder{}
	req := baseRuntimeRequest(recorder)
	req.SessionStarter = func(context.Context, SessionConfig) (*SessionResult, error) {
		return &SessionResult{DeviceID: "dev-1"}, nil
	}
	if _, err := (Runtime{}).Start(context.Background(), req); err != nil {
		t.Fatalf("Runtime.Start() error = %v", err)
	}
	if got := recorder.kinds(); !reflect.DeepEqual(got, []string{"prepared", "connecting"}) {
		t.Fatalf("unestablished runtime events = %v", got)
	}
}

func TestRuntimeReconnectDryRunReturnsWithoutLooping(t *testing.T) {
	recorder := &eventRecorder{}
	req := baseRuntimeRequest(recorder)
	req.DryRun = true
	req.Reconnect = true
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := (Runtime{}).Start(ctx, req); err != nil {
		t.Fatalf("Runtime.Start(reconnect dry-run) error = %v", err)
	}
}

type testEndpoint struct{ id string }

func (endpoint *testEndpoint) DeviceID() string { return endpoint.id }
func (*testEndpoint) IsRegistered() bool        { return true }
func (*testEndpoint) NextCSeq() uint32          { return 1 }
func (*testEndpoint) Snapshot() imsendpoint.Snapshot {
	return imsendpoint.Snapshot{}
}
func (*testEndpoint) Subscribe(
	imsendpoint.EventSubscription,
	func(imsendpoint.Event),
) func() {
	return func() {}
}
func (*testEndpoint) StartClientInvite(
	context.Context,
	string,
	imsendpoint.ClientInviteOptions,
) (*imsendpoint.ClientInviteResult, error) {
	return nil, errors.New("test endpoint has no client INVITE transport")
}
func (*testEndpoint) CancelClientInvite(
	context.Context,
	string,
	imsendpoint.InviteHandle,
	imsendpoint.ClientInviteCancelOptions,
) error {
	return errors.New("test endpoint has no client INVITE transport")
}
func (*testEndpoint) RespondInboundRequest(
	context.Context,
	string,
	imsendpoint.InboundRequestHandle,
	imsendpoint.InboundResponseOptions,
) error {
	return errors.New("test endpoint has no server transaction transport")
}
func (*testEndpoint) AnswerServerInvite(
	context.Context,
	string,
	imsendpoint.ServerInviteHandle,
	imsendpoint.ServerInviteAnswerOptions,
) (imsendpoint.DialogHandle, error) {
	return nil, errors.New("test endpoint has no server transaction transport")
}
func (*testEndpoint) RejectServerInvite(
	context.Context,
	string,
	imsendpoint.ServerInviteHandle,
	imsendpoint.ServerInviteRejectOptions,
) error {
	return errors.New("test endpoint has no server transaction transport")
}
func (*testEndpoint) CloseDialog(context.Context, string, imsendpoint.DialogHandle) error {
	return errors.New("test endpoint has no dialog transport")
}
func (*testEndpoint) SendDialogRequest(
	context.Context,
	string,
	imsendpoint.DialogHandle,
	*sip.Request,
	imsendpoint.DialogRequestOptions,
) (*sip.Response, error) {
	return nil, errors.New("test endpoint has no dialog transport")
}
func (*testEndpoint) SendReliableProvisionalPRACK(
	context.Context,
	string,
	imsendpoint.ReliableProvisionalOptions,
) error {
	return errors.New("test endpoint has no reliable provisional transport")
}

type recordingVoice struct {
	mu       sync.Mutex
	attached []imsendpoint.Endpoint
	detached int
}

func (voice *recordingVoice) AttachDevice(_ string, endpoint imsendpoint.Endpoint) error {
	voice.mu.Lock()
	voice.attached = append(voice.attached, endpoint)
	voice.mu.Unlock()
	return nil
}

func (voice *recordingVoice) DetachDevice(string) {
	voice.mu.Lock()
	voice.detached++
	voice.mu.Unlock()
}

func TestVoiceLifecycleBindingSerializesAttachAndStop(t *testing.T) {
	voice := &recordingVoice{}
	binding := &voiceLifecycleBinding{deviceID: "dev-1", voice: voice}
	endpoint := &testEndpoint{id: "dev-1"}
	var wait sync.WaitGroup
	for range 20 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			binding.AttachIfReady(endpoint)
		}()
	}
	wait.Wait()
	binding.Stop()
	binding.Stop()
	voice.mu.Lock()
	defer voice.mu.Unlock()
	if len(voice.attached) != 1 || voice.detached != 1 {
		t.Fatalf("voice attach=%d detach=%d", len(voice.attached), voice.detached)
	}
}

type blockingVoice struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	active  bool
}

func (voice *blockingVoice) AttachDevice(string, imsendpoint.Endpoint) error {
	close(voice.started)
	<-voice.release
	voice.mu.Lock()
	voice.active = true
	voice.mu.Unlock()
	return nil
}

func (voice *blockingVoice) DetachDevice(string) {
	voice.mu.Lock()
	voice.active = false
	voice.mu.Unlock()
}

func TestVoiceStopWaitsForPendingAttachCleanup(t *testing.T) {
	voice := &blockingVoice{started: make(chan struct{}), release: make(chan struct{})}
	binding := &voiceLifecycleBinding{deviceID: "dev-1", voice: voice}
	attached := make(chan struct{})
	go func() {
		binding.AttachIfReady(&testEndpoint{id: "dev-1"})
		close(attached)
	}()
	<-voice.started
	stopped := make(chan struct{})
	go func() {
		binding.Stop()
		close(stopped)
	}()
	close(voice.release)
	<-attached
	<-stopped
	voice.mu.Lock()
	defer voice.mu.Unlock()
	if voice.active {
		t.Fatal("voice endpoint remained attached after concurrent Stop")
	}
}

func TestResolveIPSec3GPPInstaller(t *testing.T) {
	userspace := &netstack.Network{}
	if got := resolveIPSec3GPPInstaller("userspace", userspace); got != userspace {
		t.Fatalf("userspace installer = %T, want supplied network", got)
	}
	if got := resolveIPSec3GPPInstaller(" USERSPACE ", nil); got != nil {
		t.Fatalf("missing userspace installer = %T, want nil", got)
	}
	if got := resolveIPSec3GPPInstaller("kernel", nil); got == nil {
		t.Fatal("kernel installer is nil")
	} else if _, ok := got.(imscore.SystemIPSec3GPPInstaller); !ok {
		t.Fatalf("kernel installer = %T, want system installer", got)
	}
}

type recordingIPSecInstaller struct {
	installs int
	cleanups int
}

func (installer *recordingIPSecInstaller) InstallIPSec3GPP(
	context.Context,
	ipsec3gpp.Policy,
) (func() error, error) {
	installer.installs++
	return func() error {
		installer.cleanups++
		return nil
	}, nil
}

func TestInstallerIMSNetworkReplacesAndCleansPolicies(t *testing.T) {
	installer := &recordingIPSecInstaller{}
	network := newInstallerIMSNetwork(nil, installer)
	if err := network.InstallIPSec3GPP(ipsec3gpp.Policy{}); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if err := network.InstallIPSec3GPP(ipsec3gpp.Policy{}); err != nil {
		t.Fatalf("second install: %v", err)
	}
	if installer.installs != 2 || installer.cleanups != 1 {
		t.Fatalf("before close installs=%d cleanups=%d", installer.installs, installer.cleanups)
	}
	if err := network.RemoveIPSec3GPP(); err != nil {
		t.Fatalf("RemoveIPSec3GPP: %v", err)
	}
	if installer.cleanups != 2 {
		t.Fatalf("after remove cleanups=%d, want 2", installer.cleanups)
	}
	if err := network.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if installer.cleanups != 2 {
		t.Fatalf("idempotent close cleanups=%d, want 2", installer.cleanups)
	}
}

type protectedUDPRecordingNetwork struct {
	imscore.IMSNetwork
	requested *net.UDPAddr
}

func (network *protectedUDPRecordingNetwork) ListenProtectedUDP(address *net.UDPAddr) (net.PacketConn, error) {
	network.requested = address
	return nil, nil
}

func TestInstallerIMSNetworkPreservesProtectedUDPCapability(t *testing.T) {
	base := &protectedUDPRecordingNetwork{
		IMSNetwork: imscore.NewSystemIMSNetwork(net.IPv4(10, 0, 0, 2)),
	}
	managed := newInstallerIMSNetwork(base, nil)
	protected, ok := managed.(protectedUDPNetwork)
	if !ok {
		t.Fatal("installer wrapper dropped protected UDP capability")
	}
	address := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 41001}
	if _, err := protected.ListenProtectedUDP(address); err != nil {
		t.Fatal(err)
	}
	if base.requested != address {
		t.Fatal("protected UDP request did not reach the underlying network")
	}
	plain := newInstallerIMSNetwork(imscore.NewSystemIMSNetwork(address.IP), nil)
	if _, ok := plain.(protectedUDPNetwork); ok {
		t.Fatal("wrapper advertised protected UDP for an unsupported network")
	}
}

func TestMissingIPSec3GPPInstallerReturnsOriginalError(t *testing.T) {
	installer := &imscore.MissingIPSec3GPPInstaller{}
	_, err := installer.InstallIPSec3GPP(context.Background(), ipsec3gpp.Policy{})
	if err == nil || err.Error() != "ipsec3gpp: installer not configured" {
		t.Fatalf("missing installer error = %v", err)
	}
}

type testSIPResultStore struct {
	smsdelivery.Store
	sipCode       int
	status        *smsdelivery.DeliveryStatus
	inboundStored bool
}

func (store *testSIPResultStore) MarkSMSDeliveryPartSIPResult(
	_ string,
	_, sipCode int,
	_, _ string,
	_ time.Time,
) error {
	store.sipCode = sipCode
	return nil
}

func (store *testSIPResultStore) GetSMSDeliveryStatus(string) (*smsdelivery.DeliveryStatus, error) {
	return store.status, nil
}

func (store *testSIPResultStore) LoadInboundFragments(
	smsdelivery.InboundFragmentOwner,
) ([]smsdelivery.StoredInboundFragment, error) {
	return nil, nil
}

func (store *testSIPResultStore) SaveInboundFragment(
	smsdelivery.InboundFragmentScope,
	smsdelivery.InboundFragment,
) (smsdelivery.InboundFragmentSaveResult, error) {
	store.inboundStored = true
	return smsdelivery.InboundFragmentSaveResult{Inserted: true}, nil
}

func (store *testSIPResultStore) DeleteInboundFragments(smsdelivery.InboundFragmentScope) error {
	return nil
}

func (store *testSIPResultStore) MarkInboundFragmentAcked(
	smsdelivery.InboundFragmentScope,
	int,
	time.Time,
) error {
	return nil
}

func (store *testSIPResultStore) MarkInboundFragmentsDegraded(
	smsdelivery.InboundFragmentScope,
	time.Time,
) error {
	store.inboundStored = true
	return nil
}

func TestDeliveryStoreAdapterPreservesOptionalSIPResults(t *testing.T) {
	store := &testSIPResultStore{}
	adapter := adaptDeliveryStore(store)
	sipResults, ok := adapter.(imscore.SMSDeliverySIPResultStore)
	if !ok {
		t.Fatal("runtimecore delivery adapter lost SIP result capability")
	}
	if err := sipResults.MarkSMSDeliveryPartSIPResult(
		"msg-1", 1, 202, "pending", "", time.Now(),
	); err != nil {
		t.Fatalf("MarkSMSDeliveryPartSIPResult: %v", err)
	}
	if store.sipCode != 202 {
		t.Fatalf("persisted SIP code = %d", store.sipCode)
	}
	fragments, ok := adapter.(imscore.SMSInboundFragmentStore)
	if !ok {
		t.Fatal("runtimecore delivery adapter lost inbound fragment capability")
	}
	if _, err := fragments.SaveInboundFragment(
		smsdelivery.InboundFragmentScope{}, smsdelivery.InboundFragment{},
	); err != nil || !store.inboundStored {
		t.Fatalf("SaveInboundFragment err=%v stored=%v", err, store.inboundStored)
	}
	store.inboundStored = false
	lifecycle, ok := adapter.(imscore.SMSInboundFragmentLifecycleStore)
	if !ok {
		t.Fatal("runtimecore delivery adapter lost fragment lifecycle capability")
	}
	if err := lifecycle.MarkInboundFragmentsDegraded(smsdelivery.InboundFragmentScope{}, time.Now()); err != nil || !store.inboundStored {
		t.Fatalf("MarkInboundFragmentsDegraded err=%v stored=%v", err, store.inboundStored)
	}
}

func TestDeliveryStoreAdapterPreservesStatusMetadata(t *testing.T) {
	createdAt := time.Date(2026, time.August, 10, 1, 58, 25, 0, time.UTC)
	updatedAt := createdAt.Add(300 * time.Millisecond)
	reportAt := updatedAt
	store := &testSIPResultStore{status: &smsdelivery.DeliveryStatus{
		MessageID: "msg-1", CreatedAt: createdAt, UpdatedAt: updatedAt,
		Parts: []smsdelivery.DeliveryPartStatus{{
			PartNo: 1, CallID: "call-1", InReplyTo: "reply-1", RPMR: 17,
			RPCauseText: "accepted", SentAt: createdAt, ReportAt: &reportAt,
			CreatedAt: createdAt, UpdatedAt: updatedAt,
		}},
	}}
	status, err := adaptDeliveryStore(store).GetSMSDeliveryStatus("msg-1")
	if err != nil {
		t.Fatal(err)
	}
	if status.CreatedAt != createdAt || status.UpdatedAt != updatedAt || len(status.Parts) != 1 {
		t.Fatalf("status metadata = %+v", status)
	}
	part := status.Parts[0]
	if part.InReplyTo != "reply-1" || part.RPMR != 17 || part.ReportAt == nil || *part.ReportAt != reportAt {
		t.Fatalf("part metadata = %+v", part)
	}
}

var _ imsendpoint.Endpoint = (*imscore.Service)(nil)

func TestCarrierPresetKeysReachSWUConfig(t *testing.T) {
	build := func(mcc, mnc string) *swu.Config {
		plan := carrierPlanFromCompatibility(carrier.ResolveEffectiveCarrierConfig(mcc, mnc))
		return BuildSWUConfig(SessionConfig{Prepared: profile.PreparedSession{CarrierPlan: plan}})
	}
	if spark := build("530", "05"); !spark.WithholdDeviceIdentity || !spark.KeepChildSAOnRekeyDecline || !spark.DeferPeerChildRekeySwitch {
		t.Fatalf("Spark NZ SWu config = %+v", spark)
	}
	if one := build("530", "01"); !one.WithholdDeviceIdentity {
		t.Fatalf("One NZ SWu config = %+v", one)
	}
	if lebara := build("234", "87"); !lebara.WithholdDeviceIdentity || lebara.KeepChildSAOnRekeyDecline || lebara.DeferPeerChildRekeySwitch {
		t.Fatalf("Lebara UK SWu config = %+v", lebara)
	}
}

type mwiPolicyStoreStub struct{}

func (mwiPolicyStoreStub) LoadMWISubscriptionPolicy(string, string) (string, int, error) {
	return imscore.MWISubscriptionAuto, 0, nil
}

func (mwiPolicyStoreStub) SaveMWISubscriptionRejection(string, string, int) error { return nil }

func TestConvertRegisterTemplateCarriesMWISwitch(t *testing.T) {
	if !convertRegisterTemplate(policy.IMSRegisterTemplate{DisableMWISubscription: true}, "tcp").DisableMWISubscription {
		t.Fatal("runtimecore dropped the carrier MWI switch")
	}
}
