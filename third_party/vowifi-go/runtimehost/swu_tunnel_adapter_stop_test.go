package runtimehost

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iniwex5/vowifi-go/engine/swu"
)

// Cellular-bound starts (#119 point 2): after Shutdown the tunnel reports done
// only once the stopped SWu session has exited, not when Stop was requested.
func TestSWuTunnelAdapterWaitDoneWaitsForStoppedSession(t *testing.T) {
	manager := &asyncStopManager{release: make(chan struct{})}
	adapter := newSWUTunnelAdapter(swuTunnelAdapterConfig{Manager: manager, DeviceID: "dev-1"})
	session := swu.NewSession(&swu.Config{})
	adapter.Session = session
	adapter.activeSession = session

	adapter.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	err := adapter.WaitDoneContext(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitDoneContext err = %v while the session still runs, want deadline exceeded", err)
	}
	close(manager.release)
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := adapter.WaitDoneContext(ctx); err != nil {
		t.Fatalf("WaitDoneContext err = %v after the session exited", err)
	}
}

type asyncStopManager struct {
	release chan struct{}
}

func (*asyncStopManager) Start(context.Context, string, *swu.Config) (*swu.Session, error) {
	return nil, errors.New("not used")
}

func (*asyncStopManager) Wait(context.Context, string, time.Duration) (swu.SessionSnapshot, error) {
	return swu.SessionSnapshot{}, errors.New("not used")
}

func (*asyncStopManager) Stop(string) error { return nil }

func (manager *asyncStopManager) StopAsync(string) (func(context.Context) error, error) {
	return func(ctx context.Context) error {
		select {
		case <-manager.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, nil
}

// A tunnel shut down before it ever started a session has nothing to wait for.
func TestSWuTunnelAdapterWaitDoneWithoutSession(t *testing.T) {
	adapter := newSWUTunnelAdapter(swuTunnelAdapterConfig{Manager: &asyncStopManager{release: make(chan struct{})}, DeviceID: "dev-1"})
	adapter.Shutdown()
	if err := adapter.WaitDoneContext(context.Background()); err != nil {
		t.Fatalf("WaitDoneContext err = %v, want nil", err)
	}
}
