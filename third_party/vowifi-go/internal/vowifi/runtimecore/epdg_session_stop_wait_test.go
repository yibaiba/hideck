package runtimecore

import (
	"context"
	"testing"
	"time"

	"github.com/iniwex5/vowifi-go/engine/swu"
)

// A start cancelled while waiting for the ePDG returns only after the SWu
// session goroutine has exited (#119: the old task must stop using the SIM
// before the next lifecycle command touches the modem).
func TestStartAndWaitEPDGCancelWaitsForSessionExit(t *testing.T) {
	manager := &lingeringSessionManager{release: make(chan struct{}), waitEntered: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() {
		_, _, err := StartAndWaitEPDG(ctx, "dev-1", "trace-1", &swu.Config{}, manager)
		returned <- err
	}()
	<-manager.waitEntered
	cancel()

	select {
	case err := <-returned:
		t.Fatalf("StartAndWaitEPDG returned %v while the session goroutine was still running", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(manager.release)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("StartAndWaitEPDG did not return after the session goroutine exited")
	}
}

type lingeringSessionManager struct {
	release     chan struct{}
	waitEntered chan struct{}
}

func (*lingeringSessionManager) Start(context.Context, string, *swu.Config) (*swu.Session, error) {
	return swu.NewSession(&swu.Config{}), nil
}

func (manager *lingeringSessionManager) Wait(ctx context.Context, _ string, _ time.Duration) (swu.SessionSnapshot, error) {
	close(manager.waitEntered)
	<-ctx.Done()
	return swu.SessionSnapshot{}, ctx.Err()
}

func (*lingeringSessionManager) Stop(string) error { return nil }

func (manager *lingeringSessionManager) StopAndWait(context.Context, string) error {
	<-manager.release
	return nil
}
