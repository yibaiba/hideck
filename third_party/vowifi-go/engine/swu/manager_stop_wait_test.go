package swu

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSessionManagerStopAndWaitWaitsForSessionGoroutine(t *testing.T) {
	m := NewSessionManager()
	runDone := make(chan struct{})
	m.devices["dev-1"] = map[string]*managedSession{DefaultSessionSlot: {runDone: runDone}}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	err := m.StopAndWait(ctx, "dev-1")
	cancel()
	if !errors.Is(err, ErrSessionStillRunning) {
		t.Fatalf("StopAndWait err = %v, want ErrSessionStillRunning", err)
	}

	runDone = make(chan struct{})
	m.devices["dev-1"] = map[string]*managedSession{DefaultSessionSlot: {runDone: runDone}}
	returned := make(chan error, 1)
	go func() { returned <- m.StopAndWait(context.Background(), "dev-1") }()
	select {
	case err := <-returned:
		t.Fatalf("StopAndWait returned %v before the session goroutine exited", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(runDone)
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("StopAndWait err = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("StopAndWait did not return after the session goroutine exited")
	}
}

// When another goroutine is already shutting a session down, Shutdown returns
// early and the session goroutine can exit first: the wait must last until
// local cleanup has finished, and report its failures.
func TestStopAsyncWaitsForLocalCleanupAndReportsIt(t *testing.T) {
	m := NewSessionManager()
	session := NewSession(&Config{}, nil)
	session.state = stateShutdown // cleanup owned by another goroutine
	runDone := make(chan struct{})
	close(runDone)
	m.devices["dev-1"] = map[string]*managedSession{DefaultSessionSlot: {session: session, runDone: runDone}}

	wait, err := m.StopAsync("dev-1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	err = wait(ctx)
	cancel()
	if !errors.Is(err, ErrSessionStillRunning) {
		t.Fatalf("wait err = %v before local cleanup finished, want ErrSessionStillRunning", err)
	}

	session.recordCleanupError(errors.New("delete route: busy"))
	session.signalDone()
	if err := wait(context.Background()); !errors.Is(err, ErrSessionCleanup) {
		t.Fatalf("wait err = %v after cleanup, want ErrSessionCleanup", err)
	}
}
