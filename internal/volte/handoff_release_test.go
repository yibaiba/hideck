package volte

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
)

func TestHandoffRejectsConcurrentLateCallQuery(t *testing.T) {
	h := &delayedRetireQueryHost{
		FakeModem: newFakeModem(), started: make(chan struct{}), release: make(chan struct{}),
	}
	c := NewControllerWithBackup(h, t.TempDir())
	if err := c.Enable(context.Background(), "wwan1"); err != nil {
		t.Fatal(err)
	}
	h.pause.Store(true)
	done := make(chan struct{})
	go func() { c.ReconcileCalls(context.Background(), "wwan1"); close(done) }()
	<-h.started
	var once sync.Once
	release := func() { once.Do(func() { close(h.release) }); <-done }
	t.Cleanup(release)
	if err := c.DisableForHandoff(context.Background(), "wwan1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Enable(context.Background(), "wwan1"); err != nil {
		t.Fatal(err)
	}
	release()
	if c.ActiveCall("wwan1") != nil {
		t.Fatal("late query populated replacement session")
	}
	c.Disable("wwan1")
}

func TestHandoffDetachesSubscriptionsAndRejectsOldCallbacks(t *testing.T) {
	c, h := newHandoffTest(t)
	oldRegistration, oldVoice := h.imsRegHandler, h.voiceHandler
	h.afterHangup = &qmi.VoiceAllCallInfo{}
	if err := c.DisableForHandoff(context.Background(), "wwan1"); err != nil {
		t.Fatal(err)
	}
	if h.IMSUnsubscribeCount != 2 || h.VoiceUnsubscribeCount != 1 {
		t.Fatalf("subscriptions remain: IMS=%d voice=%d", h.IMSUnsubscribeCount, h.VoiceUnsubscribeCount)
	}
	if err := c.Enable(context.Background(), "wwan1"); err != nil {
		t.Fatal(err)
	}
	s := c.sess["wwan1"]
	oldRegistration(&qmi.IMSARegistrationStatus{HasStatus: true, Status: qmi.IMSARegistrationStateNotRegistered})
	oldVoice(&qmi.VoiceAllCallInfo{Calls: []qmi.VoiceCallInfo{{ID: 7, State: qmiCallIncoming}}})
	if c.sess["wwan1"] != s || !c.Status("wwan1").Ready() || c.ActiveCall("wwan1") != nil {
		t.Fatal("old callbacks changed replacement session")
	}
	c.Disable("wwan1")
}

type closeFailurePCM struct {
	memPCM
	failure error
}

func (p *closeFailurePCM) Close() error { _ = p.memPCM.Close(); return p.failure }

func TestHandoffMediaCloseFailureIsNotForgottenOnRetry(t *testing.T) {
	c, h := newHandoffTest(t)
	h.afterHangup = &qmi.VoiceAllCallInfo{}
	failure := errors.New("PCM close failed")
	m, err := startCallMedia("", &closeFailurePCM{failure: failure}, true)
	if err != nil {
		t.Fatal(err)
	}
	c.media.put("old-call", m)
	s := c.sess["wwan1"]
	for range 2 {
		if err := c.DisableForHandoff(context.Background(), "wwan1"); !errors.Is(err, failure) {
			t.Fatalf("cleanup error disappeared: %v", err)
		}
		if c.sess["wwan1"] != s || c.media.get("old-call") != m {
			t.Fatal("failed cleanup lost ownership")
		}
	}
}

type retryStopRunner struct {
	fakeRunner
	failure error
}

func (r *retryStopRunner) Stop(pid int) error { r.stops = append(r.stops, pid); return r.failure }

func TestHandoffAudioStopFailureCanBeRetried(t *testing.T) {
	c, h := newHandoffTest(t)
	h.afterHangup = &qmi.VoiceAllCallInfo{}
	failure := errors.New("helper stop failed")
	runner := &retryStopRunner{failure: failure}
	c.audio = NewAudioRuntime(AudioIdentity{}, runner, nil, "helper")
	c.audio.calls["old-call"] = audioCall{deviceID: "wwan1", pid: 123}
	if err := c.DisableForHandoff(context.Background(), "wwan1"); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if _, ok := c.audio.Owner("old-call"); !ok {
		t.Fatal("failed stop lost helper ownership")
	}
	runner.failure = nil
	if err := c.DisableForHandoff(context.Background(), "wwan1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.audio.Owner("old-call"); ok || len(runner.stops) != 2 {
		t.Fatal("retry did not finish helper cleanup")
	}
}
