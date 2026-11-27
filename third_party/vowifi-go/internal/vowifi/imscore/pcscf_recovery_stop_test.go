package imscore

import (
	"testing"
	"time"
)

// #119 point 2: the 503 P-CSCF recovery may register (AKA on the SIM); IMS
// stop must cancel it and wait for it instead of leaving it running.
func TestStopCurrentWaitsForPCSCFRecovery(t *testing.T) {
	s := newPortSTimeoutTestService(t)
	s.registerMu.Lock() // hold the recovery at its first step
	s.observeInitialInvite503(&sipResponse{StatusCode: 503})

	stopped := make(chan struct{})
	go func() {
		s.StopCurrent()
		close(stopped)
	}()
	select {
	case <-stopped:
		s.registerMu.Unlock()
		t.Fatal("StopCurrent returned while the P-CSCF recovery was still running")
	case <-time.After(100 * time.Millisecond):
	}
	s.registerMu.Unlock()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("StopCurrent did not return after the P-CSCF recovery finished")
	}
}

// Every P-CSCF recovery started through startPCSCFRecovery (503, port-S
// reset, MT report 488) is waited for by StopCurrent and sees its context
// cancelled.
func TestStopCurrentCancelsAndWaitsForAnyPCSCFRecovery(t *testing.T) {
	s := newPortSTimeoutTestService(t)
	cancelled := make(chan struct{})
	release := make(chan struct{})
	if !s.startPCSCFRecovery(func() {
		ctx, cancel := s.recoveryRegistrationContext()
		defer cancel()
		<-ctx.Done()
		close(cancelled)
		<-release
	}) {
		t.Fatal("recovery was not started on a running service")
	}
	stopped := make(chan struct{})
	go func() {
		s.StopCurrent()
		close(stopped)
	}()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("stopping the service did not cancel the recovery registration")
	}
	select {
	case <-stopped:
		t.Fatal("StopCurrent returned while the recovery was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("StopCurrent did not return after the recovery finished")
	}
	if s.startPCSCFRecovery(func() {}) {
		t.Fatal("a recovery was started after the service stopped")
	}
}

// Host release and runner cleanup may stop the same service at once.
func TestConcurrentStopCurrentDoesNotPanic(t *testing.T) {
	s := newPortSTimeoutTestService(t)
	done := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			s.StopCurrent()
			done <- struct{}{}
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent StopCurrent did not return")
		}
	}
}
