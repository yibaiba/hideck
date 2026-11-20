package device

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost"
	"github.com/iniwex5/vowifi-go/runtimehost/messaging"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/modem"
	"github.com/yibaiba/hideck/pkg/smscodec"
)

func TestSendVoWiFiSMSWhenReadySendsImmediately(t *testing.T) {
	runtime := &fakeVoWiFiSMSRuntime{state: runtimehost.State{SMSReady: true}}
	runtime.send = func(context.Context, string, string, messaging.SendOptions) (messaging.SendOutcome, error) {
		return messaging.SendOutcome{MessageID: "msg-1"}, nil
	}
	outcome, err := sendVoWiFiSMSWhenReady(context.Background(), voWiFiSMSSendRequest{
		DeviceID: "wwan0", To: "888", Text: "BAL", Updates: make(chan runtimehost.State),
		Runtime: func() voWiFiSMSRuntime { return runtime },
	})
	if err != nil || outcome.MessageID != "msg-1" || runtime.sendCount() != 1 {
		t.Fatalf("send outcome=%+v err=%v count=%d", outcome, err, runtime.sendCount())
	}
}

func TestSendVoWiFiSMSWhenMOReadyDoesNotWaitForMTReceiver(t *testing.T) {
	runtime := &fakeVoWiFiSMSRuntime{state: runtimehost.State{
		IMSReady: true, SMSMOReady: true, SMSReadyReason: "IMS SMS receiver is not ready",
	}}
	runtime.send = func(context.Context, string, string, messaging.SendOptions) (messaging.SendOutcome, error) {
		return messaging.SendOutcome{MessageID: "msg-mo-ready"}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	outcome, err := sendVoWiFiSMSWhenReady(ctx, voWiFiSMSSendRequest{
		DeviceID: "wwan0", To: "+15551234567", Text: "test", Updates: make(chan runtimehost.State),
		Runtime: func() voWiFiSMSRuntime { return runtime },
	})
	if err != nil || outcome.MessageID != "msg-mo-ready" || runtime.sendCount() != 1 {
		t.Fatalf("MO-ready send outcome=%+v err=%v count=%d", outcome, err, runtime.sendCount())
	}
}

func TestSendVoWiFiSMSWhenReadyWaitsAcrossRecovery(t *testing.T) {
	failedRuntime := &fakeVoWiFiSMSRuntime{state: runtimehost.State{
		SMSReadyReason: "IMS registration is not ready",
	}}
	recoveredRuntime := &fakeVoWiFiSMSRuntime{state: runtimehost.State{
		SMSReady: true, SMSReadyReason: "IMS SMS receiver ready",
	}}
	recoveredRuntime.send = func(context.Context, string, string, messaging.SendOptions) (messaging.SendOutcome, error) {
		return messaging.SendOutcome{MessageID: "msg-recovered"}, nil
	}
	var runtimeMu sync.Mutex
	currentRuntime := voWiFiSMSRuntime(failedRuntime)
	updates := make(chan runtimehost.State, 1)
	checked := make(chan struct{}, 1)
	result := make(chan voWiFiSMSSendResult, 1)
	go func() {
		outcome, err := sendVoWiFiSMSWhenReady(context.Background(), voWiFiSMSSendRequest{
			DeviceID: "wwan1", To: "888", Text: "BAL", Updates: updates,
			Runtime: func() voWiFiSMSRuntime {
				runtimeMu.Lock()
				defer runtimeMu.Unlock()
				select {
				case checked <- struct{}{}:
				default:
				}
				return currentRuntime
			},
		})
		result <- voWiFiSMSSendResult{outcome: outcome, err: err}
	}()
	select {
	case <-checked:
	case <-time.After(time.Second):
		t.Fatal("send did not inspect the recovering runtime")
	}
	runtimeMu.Lock()
	currentRuntime = recoveredRuntime
	runtimeMu.Unlock()
	updates <- recoveredRuntime.State()
	select {
	case got := <-result:
		if got.err != nil || got.outcome.MessageID != "msg-recovered" ||
			failedRuntime.sendCount() != 0 || recoveredRuntime.sendCount() != 1 {
			t.Fatalf("send result=%+v err=%v old/new count=%d/%d", got.outcome, got.err,
				failedRuntime.sendCount(), recoveredRuntime.sendCount())
		}
	case <-time.After(time.Second):
		t.Fatal("send did not resume after SMS readiness recovered")
	}
}

func TestSendVoWiFiSMSWhenReadySurfacesRecoveryTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := sendVoWiFiSMSWhenReady(ctx, voWiFiSMSSendRequest{
		DeviceID: "wwan1", Updates: make(chan runtimehost.State),
		Runtime: func() voWiFiSMSRuntime { return nil },
	})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "等待短信恢复失败") {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestSendVoWiFiSMSWhenReadyDoesNotWaitForMissingSMSC(t *testing.T) {
	runtime := &fakeVoWiFiSMSRuntime{state: runtimehost.State{
		IMSReady: true, SMSReadyReason: "IMS SMSC is not configured",
	}}
	runtime.send = func(context.Context, string, string, messaging.SendOptions) (messaging.SendOutcome, error) {
		return messaging.SendOutcome{}, messaging.ErrSMSNotReady
	}
	_, err := sendVoWiFiSMSWhenReady(context.Background(), voWiFiSMSSendRequest{
		DeviceID: "wwan0", Updates: make(chan runtimehost.State),
		Runtime: func() voWiFiSMSRuntime { return runtime },
	})
	if !errors.Is(err, messaging.ErrSMSNotReady) || runtime.sendCount() != 1 {
		t.Fatalf("missing SMSC error=%v count=%d", err, runtime.sendCount())
	}
}

func TestShouldRouteSMSViaVoWiFiDuringConfiguredRecovery(t *testing.T) {
	tests := []struct {
		name     string
		config   config.DeviceConfig
		expected bool
	}{
		{name: "wifi recovery", config: config.DeviceConfig{VoWiFiEnabled: true, PhoneMode: "wifi"}, expected: true},
		{name: "cellular always recovery", config: config.DeviceConfig{VoWiFiEnabled: true, PhoneMode: "cellular", DataStrategy: "always"}, expected: true},
		{name: "cellular on demand idle", config: config.DeviceConfig{VoWiFiEnabled: true, PhoneMode: "cellular", DataStrategy: "on_demand"}, expected: false},
		{name: "native volte uses modem SMS", config: config.DeviceConfig{VoWiFiEnabled: true, PhoneMode: "volte"}, expected: false},
		{name: "disabled", config: config.DeviceConfig{VoWiFiEnabled: false}, expected: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := NewPool(&config.Config{})
			defer pool.cancel()
			pool.workers["wwan0"] = &Worker{ID: "wwan0", Config: tt.config}
			if got := pool.ShouldRouteSMSViaVoWiFi("wwan0"); got != tt.expected {
				t.Fatalf("ShouldRouteSMSViaVoWiFi() = %t, want %t", got, tt.expected)
			}
		})
	}
}

func TestSendRoutedSMSDoesNotGuessCountryCodeForEitherLeg(t *testing.T) {
	initDevicePhoneNumberTestDB(t)
	pool := NewPool(&config.Config{})
	defer pool.cancel()
	worker := &Worker{ID: "wwan0", Config: config.DeviceConfig{VoWiFiEnabled: true, PhoneMode: "wifi"}}
	worker.state.Identity.NativeMCC = "460"
	setWorkerSMSIdentity(worker, SMSIdentity{ICCID: "8944000000000000401", IMSI: "460000000000401"})
	pool.workers[worker.ID] = worker
	var imsTo, csTo string
	pool.SetRoutedSMSTestSenders(
		func(_ context.Context, _, to, _ string, _ smscodec.SubmitOptions) (messaging.SendOutcome, error) {
			imsTo = to
			return messaging.SendOutcome{}, messaging.ErrSMSNotReady
		},
		func(_, to, _ string) error { csTo = to; return nil },
	)
	result, err := pool.SendRoutedSMS(context.Background(), worker, "13123456789", "hello", smscodec.SubmitOptions{})
	if err != nil || !result.FellBackToCS || result.Destination != "13123456789" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if imsTo != result.Destination || csTo != result.Destination {
		t.Fatalf("VoWiFi destination=%q, CS destination=%q", imsTo, csTo)
	}
}

func TestSendRoutedSMSKeepsExplicitInternationalDestination(t *testing.T) {
	initDevicePhoneNumberTestDB(t)
	pool := NewPool(&config.Config{})
	defer pool.cancel()
	worker := &Worker{ID: "wwan0", Config: config.DeviceConfig{VoWiFiEnabled: true, PhoneMode: "wifi"}}
	setWorkerSMSIdentity(worker, SMSIdentity{ICCID: "8944000000000000402", IMSI: "460000000000402"})
	pool.workers[worker.ID] = worker
	var sentTo string
	pool.SetRoutedSMSTestSenders(func(_ context.Context, _, to, _ string, _ smscodec.SubmitOptions) (messaging.SendOutcome, error) {
		sentTo = to
		return messaging.SendOutcome{}, nil
	}, nil)
	result, err := pool.SendRoutedSMS(context.Background(), worker, " +8613123456789 ", "hello", smscodec.SubmitOptions{})
	if err != nil || sentTo != "+8613123456789" || result.Destination != sentTo {
		t.Fatalf("result=%+v sentTo=%q err=%v", result, sentTo, err)
	}
}

func TestSendRoutedSMSReturnsIdentityCapturedBeforeSend(t *testing.T) {
	initDevicePhoneNumberTestDB(t)
	pool := NewPool(&config.Config{})
	defer pool.cancel()
	worker := &Worker{ID: "wwan0", Config: config.DeviceConfig{VoWiFiEnabled: true, PhoneMode: "wifi"}}
	setWorkerSMSIdentity(worker, SMSIdentity{ICCID: "8944000000000000111", IMSI: "234150000000111"})
	pool.workers[worker.ID] = worker
	pool.SetRoutedSMSTestSenders(func(_ context.Context, _, _, _ string, _ smscodec.SubmitOptions) (messaging.SendOutcome, error) {
		setWorkerSMSIdentity(worker, SMSIdentity{ICCID: "8944000000000000222", IMSI: "234150000000222"})
		return messaging.SendOutcome{}, errors.New("send failed")
	}, nil)

	result, err := pool.SendRoutedSMS(context.Background(), worker, "+44123456789", "hello", smscodec.SubmitOptions{})
	if err == nil {
		t.Fatal("SendRoutedSMS() unexpectedly succeeded")
	}
	if !result.IdentityRead || result.Identity.ICCID != "8944000000000000111" || result.Identity.IMSI != "234150000000111" {
		t.Fatalf("captured identity=%+v read=%t", result.Identity, result.IdentityRead)
	}
}

func TestSendRoutedSMSStopsWhenIdentityCannotBeResolved(t *testing.T) {
	initDevicePhoneNumberTestDB(t)
	pool := NewPool(&config.Config{})
	defer pool.cancel()
	worker := &Worker{ID: "wwan0", Config: config.DeviceConfig{VoWiFiEnabled: true, PhoneMode: "wifi"}}
	pool.workers[worker.ID] = worker
	called := false
	pool.SetRoutedSMSTestSenders(func(context.Context, string, string, string, smscodec.SubmitOptions) (messaging.SendOutcome, error) {
		called = true
		return messaging.SendOutcome{}, nil
	}, nil)

	result, err := pool.SendRoutedSMS(context.Background(), worker, "+44123456789", "hello", smscodec.SubmitOptions{})
	if err == nil || result.IdentityRead || called {
		t.Fatalf("result=%+v err=%v called=%t", result, err, called)
	}
}

func TestShouldFallbackVoWiFiSMSToCS(t *testing.T) {
	tests := []struct {
		name    string
		outcome messaging.SendOutcome
		err     error
		want    bool
	}{
		{name: "success", outcome: messaging.SendOutcome{RecommendCSFallback: true}},
		{name: "not ready", err: fmt.Errorf("wrap: %w", messaging.ErrSMSNotReady), want: true},
		{name: "recommended", outcome: messaging.SendOutcome{SIPCode: 503, RecommendCSFallback: true}, err: errors.New("503"), want: true},
		{name: "rejected 403", outcome: messaging.SendOutcome{SIPCode: 403}, err: errors.New("403")},
		{name: "report timeout", outcome: messaging.SendOutcome{SIPCode: 0}, err: errors.New("report timeout")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ShouldFallbackVoWiFiSMSToCS(test.outcome, test.err); got != test.want {
				t.Fatalf("ShouldFallbackVoWiFiSMSToCS() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestSendRoutedSMSFallsBackOnceWithoutDoubleSend(t *testing.T) {
	vowifiCalls, csCalls := 0, 0
	result, err := sendRoutedSMS(true,
		func() (messaging.SendOutcome, error) {
			vowifiCalls++
			return messaging.SendOutcome{MessageID: "ims-1", SIPCode: 503, RecommendCSFallback: true, DeliveryState: "failed"}, errors.New("503")
		},
		func() error { csCalls++; return nil },
	)
	if err != nil || !result.FellBackToCS || result.Via != RoutedSMSViaCS || result.Outcome.MessageID != "ims-1" {
		t.Fatalf("fallback result=%+v err=%v", result, err)
	}
	if vowifiCalls != 1 || csCalls != 1 {
		t.Fatalf("calls vowifi=%d cs=%d", vowifiCalls, csCalls)
	}
}

func TestSendRoutedSMSDoesNotFallbackAfterIMSAccept(t *testing.T) {
	csCalls := 0
	result, err := sendRoutedSMS(true,
		func() (messaging.SendOutcome, error) {
			return messaging.SendOutcome{MessageID: "ims-1", SIPCode: 403, RecommendCSFallback: false, DeliveryState: "failed"}, errors.New("403")
		},
		func() error { csCalls++; return nil },
	)
	if err == nil || result.FellBackToCS || result.Via != RoutedSMSViaVoWiFi || csCalls != 0 {
		t.Fatalf("rejected fallback result=%+v err=%v cs=%d", result, err, csCalls)
	}
}

func TestSendRoutedSMSKeepsVoWiFiErrorWhenCSAlsoFails(t *testing.T) {
	imsErr := errors.New("503")
	result, err := sendRoutedSMS(true,
		func() (messaging.SendOutcome, error) {
			return messaging.SendOutcome{RecommendCSFallback: true}, imsErr
		},
		func() error { return errors.New("cs down") },
	)
	if !errors.Is(err, imsErr) || !result.FellBackToCS || result.Via != RoutedSMSViaVoWiFi {
		t.Fatalf("dual failure result=%+v err=%v", result, err)
	}
}

func TestSendRoutedSMSKeepsUnknownCSFallbackResult(t *testing.T) {
	imsErr := errors.New("503")
	csErr := fmt.Errorf("CS submit: %w", modem.ErrSMSSubmitUnknown)
	calls := 0
	result, err := sendRoutedSMS(true,
		func() (messaging.SendOutcome, error) {
			return messaging.SendOutcome{MessageID: "ims-1", RecommendCSFallback: true, DeliveryState: "failed"}, imsErr
		},
		func() error { calls++; return csErr },
	)
	if err != csErr || !result.FellBackToCS || result.Via != RoutedSMSViaCS ||
		result.Outcome.DeliveryState != "unknown" || result.Outcome.MessageID != "" || calls != 1 {
		t.Fatalf("fallback result=%+v err=%v CS calls=%d", result, err, calls)
	}
}

type voWiFiSMSSendResult struct {
	outcome messaging.SendOutcome
	err     error
}

type fakeVoWiFiSMSRuntime struct {
	mu    sync.Mutex
	state runtimehost.State
	sends int
	send  func(context.Context, string, string, messaging.SendOptions) (messaging.SendOutcome, error)
}

func (f *fakeVoWiFiSMSRuntime) State() runtimehost.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeVoWiFiSMSRuntime) SendSMSWithOptions(
	ctx context.Context,
	to string,
	text string,
	options messaging.SendOptions,
) (messaging.SendOutcome, error) {
	f.mu.Lock()
	f.sends++
	send := f.send
	f.mu.Unlock()
	return send(ctx, to, text, options)
}

func (f *fakeVoWiFiSMSRuntime) sendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sends
}
