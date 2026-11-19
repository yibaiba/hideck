package device

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost"
	"github.com/iniwex5/vowifi-go/runtimehost/messaging"
	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/modem"
	"github.com/yibaiba/hideck/internal/outbound"
	"github.com/yibaiba/hideck/pkg/smscodec"
)

type recordingOutboundLimiter struct {
	requests []outbound.Request
	err      error
	after    func()
}

func (l *recordingOutboundLimiter) Consume(_ context.Context, req outbound.Request) error {
	l.requests = append(l.requests, req)
	if l.after != nil {
		l.after()
	}
	return l.err
}

func outboundTestWorker(t *testing.T) (*Pool, *Worker, *recordingOutboundLimiter) {
	t.Helper()
	p := NewPool(&config.Config{})
	w := &Worker{ID: "dev", Pool: p}
	setWorkerSMSIdentity(w, SMSIdentity{ICCID: "123", IMSI: "234150123456789"})
	p.AttachWorkerForTest(w)
	l := &recordingOutboundLimiter{}
	p.SetOutboundLimiter(l)
	return p, w, l
}

func TestRoutedSMSChargesMultipartOnceAcrossFallback(t *testing.T) {
	p, w, l := outboundTestWorker(t)
	w.Config.VoWiFiEnabled = true
	w.Config.PhoneMode = "wifi"
	ims, cs := 0, 0
	p.SetRoutedSMSTestSenders(func(context.Context, string, string, string, smscodec.SubmitOptions) (messaging.SendOutcome, error) {
		ims++
		return messaging.SendOutcome{}, messaging.ErrSMSNotReady
	}, func(string, string, string) error { cs++; return nil })
	text := strings.Repeat("测", 71)
	result, err := p.SendRoutedSMS(context.Background(), w, "+44123456789", text, smscodec.SubmitOptions{})
	if err != nil || !result.FellBackToCS || ims != 1 || cs != 1 {
		t.Fatalf("result=%+v err=%v ims=%d cs=%d", result, err, ims, cs)
	}
	if len(l.requests) != 1 || l.requests[0].Units != 2 || l.requests[0].ICCID != "123" {
		t.Fatalf("charges=%+v", l.requests)
	}
}

func TestAllSMSAndTimedCallEntrypointsRespectLimit(t *testing.T) {
	p, w, l := outboundTestWorker(t)
	l.err = &outbound.LimitedError{Kind: outbound.SMS, RetryAfter: time.Second}
	sent := false
	p.SetRoutedSMSTestSenders(nil, func(string, string, string) error { sent = true; return nil })
	_, routedErr := p.SendRoutedSMS(context.Background(), w, "10010", "test", smscodec.SubmitOptions{})
	_, imsErr := p.SendVoWiFiSMSWithOptions(context.Background(), w.ID, "10010", "test", smscodec.SubmitOptions{})
	csErr := w.SendSMS("10010", "test")
	for _, err := range []error{routedErr, imsErr, csErr} {
		if !errors.Is(err, l.err) {
			t.Fatalf("entrypoint bypassed limit: %v", err)
		}
	}
	if sent || len(l.requests) != 3 {
		t.Fatalf("sent=%v charges=%d", sent, len(l.requests))
	}
	if err := p.AuthorizeOutboundCall(context.Background(), w.ID, "10010"); !errors.Is(err, l.err) {
		t.Fatal(err)
	}
	if l.requests[3].Kind != outbound.Call || l.requests[3].Units != 1 {
		t.Fatalf("call charge=%+v", l.requests[3])
	}
	if _, err := p.SimulateCallWithCellularData(context.Background(), w.ID, voicehost.SimulateCallRequest{Callee: "10010"}); !errors.Is(err, l.err) {
		t.Fatalf("timed/bot call bypassed quota: %v", err)
	}
}

func TestOutboundInvalidInputAndSIMReplacement(t *testing.T) {
	p, w, l := outboundTestWorker(t)
	if _, err := p.SendRoutedSMS(context.Background(), w, "138abc", "test", smscodec.SubmitOptions{}); err == nil {
		t.Fatal("invalid number accepted")
	}
	if err := p.AuthorizeOutboundCall(context.Background(), w.ID, "ATD123;"); err == nil {
		t.Fatal("invalid call accepted")
	}
	if len(l.requests) != 0 {
		t.Fatal("invalid input spent quota")
	}
	l.after = func() { setWorkerSMSIdentity(w, SMSIdentity{ICCID: "456"}) }
	sent := false
	p.SetRoutedSMSTestSenders(nil, func(string, string, string) error { sent = true; return nil })
	if _, err := p.SendRoutedSMS(context.Background(), w, "10010", "test", smscodec.SubmitOptions{}); err == nil || sent {
		t.Fatalf("SIM replacement: err=%v sent=%v", err, sent)
	}
}

func TestRoutedSMSCannotFallbackOnReplacementSIM(t *testing.T) {
	p, w, _ := outboundTestWorker(t)
	w.Config.VoWiFiEnabled, w.Config.PhoneMode = true, "wifi"
	cs := false
	p.SetRoutedSMSTestSenders(func(context.Context, string, string, string, smscodec.SubmitOptions) (messaging.SendOutcome, error) {
		setWorkerSMSIdentity(w, SMSIdentity{ICCID: "456"})
		return messaging.SendOutcome{}, messaging.ErrSMSNotReady
	}, func(string, string, string) error { cs = true; return nil })
	if _, err := p.SendRoutedSMS(context.Background(), w, "10010", "test", smscodec.SubmitOptions{}); err == nil || cs {
		t.Fatalf("fallback after SIM swap: err=%v cs=%v", err, cs)
	}
}

func TestQueuedSMSSIMReplacementStopsBeforeNewRuntimeSend(t *testing.T) {
	p, w, _ := outboundTestWorker(t)
	check := p.outboundOwnerCheck(w)
	updates := make(chan runtimehost.State, 1)
	updates <- runtimehost.State{}
	oldRuntime := &fakeVoWiFiSMSRuntime{}
	newRuntime := &fakeVoWiFiSMSRuntime{state: runtimehost.State{SMSMOReady: true}}
	reads := 0
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := sendVoWiFiSMSWhenReady(ctx, voWiFiSMSSendRequest{
		Check: check, DeviceID: w.ID, To: "10010", Text: "test", Updates: updates,
		Runtime: func() voWiFiSMSRuntime {
			reads++
			if reads == 1 {
				setWorkerSMSIdentity(w, SMSIdentity{ICCID: "456"})
				return oldRuntime
			}
			return newRuntime
		},
	})
	if err == nil || errors.Is(err, context.DeadlineExceeded) || newRuntime.sendCount() != 0 {
		t.Fatalf("queued send crossed SIM: err=%v sent=%d", err, newRuntime.sendCount())
	}
}

func TestML307CCSSMSChecksLiveSIMBeforeSubmitting(t *testing.T) {
	for _, tc := range []struct {
		name         string
		iccid, imsi  string
		simErr, want error
		simAbsent    bool
	}{
		{name: "matching", iccid: "card-old", imsi: "imsi-old"},
		{name: "different_card", iccid: "card-new", imsi: "imsi-new", want: ErrSMSIdentityConflict},
		{name: "same_iccid_different_imsi", iccid: "card-old", imsi: "imsi-new", want: ErrSMSIdentityConflict},
		{name: "incomplete", iccid: "card-old", want: ErrSMSIdentityUnknown},
		{name: "absent", simAbsent: true, want: ErrSMSIdentityUnknown},
		{name: "pin_required", iccid: "card-old", imsi: "imsi-old", simErr: modem.ErrSIMPINRequired, want: modem.ErrSIMPINRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, w, be := newML307CIdentityWorker(t)
			limiter := &recordingOutboundLimiter{}
			p.SetOutboundLimiter(limiter)
			be.liveICCID, be.liveIMSI, be.simErr, be.simAbsent = tc.iccid, tc.imsi, tc.simErr, tc.simAbsent
			if err := w.SendSMS("10010", "test"); !errors.Is(err, tc.want) {
				t.Fatalf("SendSMS() = %v, want %v", err, tc.want)
			}
			count := 0
			if tc.want == nil {
				count = 1
			}
			if be.sends != count || len(limiter.requests) != 1 || limiter.requests[0].ICCID != "card-old" {
				t.Fatalf("sends=%d want %d; charges=%+v want one on intended card", be.sends, count, limiter.requests)
			}
			if tc.want != nil && !w.SIMIdentityConvergenceMatches("", 0) {
				t.Fatal("confirmed SIM state did not invalidate cached readiness")
			}
		})
	}
}

func TestML307CCSSMSRejectsSilentSIMSwapDuringAuthorization(t *testing.T) {
	p, w, be := newML307CIdentityWorker(t)
	be.liveICCID, be.liveIMSI = "card-old", "imsi-old"
	limiter := &recordingOutboundLimiter{after: func() {
		be.liveICCID, be.liveIMSI = "card-new", "imsi-new"
	}}
	p.SetOutboundLimiter(limiter)
	if err := w.SendSMS("10010", "test"); !errors.Is(err, ErrSMSIdentityConflict) {
		t.Fatalf("SendSMS() = %v, want SIM conflict", err)
	}
	if be.sends != 0 || len(limiter.requests) != 1 || limiter.requests[0].ICCID != "card-old" {
		t.Fatalf("wrong send or quota owner: sends=%d charges=%+v", be.sends, limiter.requests)
	}
}

type directSMSSelectorBackend struct {
	workerStatusBackendStub
	sends int
}

func (b *directSMSSelectorBackend) SendSMS(context.Context, string, string) error {
	b.sends++
	return nil
}

func TestDirectSMSMatchesICCIDWithoutIMSI(t *testing.T) {
	_, w, limiter := outboundTestWorker(t)
	backend := &directSMSSelectorBackend{workerStatusBackendStub: workerStatusBackendStub{mode: "qmi"}}
	w.Backend = backend
	setWorkerSMSIdentity(w, SMSIdentity{ICCID: "123"})
	err := w.SendSMSForIdentity(context.Background(), "10010", "test", smscodec.SubmitOptions{}, SMSIdentity{ICCID: "123f"})
	if err != nil || backend.sends != 1 || len(limiter.requests) != 1 || limiter.requests[0].ICCID != "123" {
		t.Fatalf("send error=%v, sends=%d, quota=%+v", err, backend.sends, limiter.requests)
	}
}

func TestRoutedSMSReportsStaleSelectorBeforeFallback(t *testing.T) {
	p, w, limiter := outboundTestWorker(t)
	w.Config.VoWiFiEnabled, w.Config.PhoneMode = true, "wifi"
	cs := false
	p.SetRoutedSMSTestSenders(func(context.Context, string, string, string, smscodec.SubmitOptions) (messaging.SendOutcome, error) {
		setWorkerSMSIdentity(w, SMSIdentity{ICCID: "456", IMSI: "234150123456789"})
		return messaging.SendOutcome{}, messaging.ErrSMSNotReady
	}, func(string, string, string) error { cs = true; return nil })
	_, err := p.SendRoutedSMSForIdentity(context.Background(), w, "10010", "test", smscodec.SubmitOptions{}, SMSIdentity{ICCID: "123"})
	if !errors.Is(err, ErrSMSSelectorMismatch) || cs || len(limiter.requests) != 1 || limiter.requests[0].ICCID != "123" {
		t.Fatalf("fallback: error=%v CS sent=%v quota=%+v", err, cs, limiter.requests)
	}
}
