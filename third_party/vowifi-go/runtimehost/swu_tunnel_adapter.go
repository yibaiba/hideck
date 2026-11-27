package runtimehost

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/iniwex5/vowifi-go/engine/swu"
	"github.com/iniwex5/vowifi-go/internal/vowifi/epdg"
	"github.com/iniwex5/vowifi-go/internal/vowifi/logging"
)

type swuTunnelAdapter struct {
	*swu.Session
	manager       epdgSessionManager
	deviceID      string
	config        *swu.Config
	timeout       time.Duration
	connectMu     sync.Mutex
	lifecycleMu   sync.Mutex
	activeSession *swu.Session
	connectCancel context.CancelFunc
	closed        bool
	// stopWait is the stop of the last session, published under lifecycleMu
	// before the stop is issued.
	stopWait *pendingStop
}

type epdgSessionManager interface {
	Start(context.Context, string, *swu.Config) (*swu.Session, error)
	Wait(context.Context, string, time.Duration) (swu.SessionSnapshot, error)
	Stop(string) error
}

type swuTunnelAdapterConfig struct {
	Manager              epdgSessionManager
	DeviceID             string
	SessionConfig        *swu.Config
	EstablishmentTimeout time.Duration
}

func newSWUTunnelAdapter(config swuTunnelAdapterConfig) *swuTunnelAdapter {
	return &swuTunnelAdapter{
		manager: config.Manager, deviceID: config.DeviceID,
		config: config.SessionConfig, timeout: config.EstablishmentTimeout,
	}
}

func (adapter *swuTunnelAdapter) Connect(ctx context.Context) (err error) {
	adapter.connectMu.Lock()
	defer adapter.connectMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	connectCtx, cancel := context.WithCancel(ctx)
	if err := adapter.beginConnect(cancel); err != nil {
		cancel()
		return err
	}
	// An established SWu session inherits connectCtx. Shutdown owns that
	// context after success; only failed establishment may cancel it here.
	defer func() {
		if err != nil {
			adapter.finishConnect(cancel)
		}
	}()

	err = adapter.connectOnce(connectCtx)
	if !epdg.ShouldRetryFreshTunnel(connectCtx, err) {
		return err
	}
	// First IKE/SOCKS5 UDP Associate often drops the first datagrams.
	// A new session (what the UI reconnect button does) usually succeeds.
	logging.Info("SWu first ePDG wait timed out; retrying with a fresh tunnel",
		"device", adapter.deviceID)
	err = adapter.connectOnce(connectCtx)
	return err
}

func (adapter *swuTunnelAdapter) connectOnce(ctx context.Context) error {
	session, err := adapter.startSession(ctx)
	if err != nil {
		return err
	}
	if _, err := adapter.manager.Wait(ctx, adapter.deviceID, adapter.timeout); err != nil {
		adapter.stopSession(session)
		return err
	}
	return nil
}

func (adapter *swuTunnelAdapter) Shutdown() {
	adapter.lifecycleMu.Lock()
	if adapter.closed {
		adapter.lifecycleMu.Unlock()
		return
	}
	adapter.closed = true
	cancel := adapter.connectCancel
	adapter.connectCancel = nil
	session := adapter.activeSession
	adapter.activeSession = nil
	var stop *pendingStop
	if session != nil {
		stop = newPendingStop()
		adapter.stopWait = stop
	}
	adapter.lifecycleMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if stop != nil {
		stop.issue(adapter.manager, adapter.deviceID)
	}
}

// WaitDoneContext waits, after a stop, until the stopped session goroutine has
// returned and its local cleanup is done; before any stop it waits for the
// current session as before. Without a session there is nothing to wait for.
func (adapter *swuTunnelAdapter) WaitDoneContext(ctx context.Context) error {
	adapter.lifecycleMu.Lock()
	stop, session := adapter.stopWait, adapter.Session
	adapter.lifecycleMu.Unlock()
	if stop != nil {
		return stop.Wait(ctx)
	}
	if session == nil {
		return nil
	}
	return session.WaitDoneContext(ctx)
}

// pendingStop is published under lifecycleMu before the stop is issued, so no
// WaitDoneContext can miss it. It waits until the stop has been issued, then
// until the stopped sessions have exited and finished local cleanup.
type pendingStop struct {
	issued chan struct{}
	wait   func(context.Context) error
}

func newPendingStop() *pendingStop {
	return &pendingStop{issued: make(chan struct{})}
}

func (p *pendingStop) issue(manager epdgSessionManager, deviceID string) {
	p.wait = stopSessionAsync(manager, deviceID)
	close(p.issued)
}

func (p *pendingStop) Wait(ctx context.Context) error {
	select {
	case <-p.issued:
	case <-ctx.Done():
		return ctx.Err()
	}
	if p.wait == nil {
		return nil
	}
	return p.wait(ctx)
}

// stopSessionAsync stops the device's sessions and returns a wait for their
// exit; a manager without StopAsync falls back to the plain Stop.
func stopSessionAsync(manager epdgSessionManager, deviceID string) func(context.Context) error {
	if async, ok := manager.(interface {
		StopAsync(string) (func(context.Context) error, error)
	}); ok {
		wait, err := async.StopAsync(deviceID)
		if err == nil {
			return wait
		}
		if !strings.Contains(err.Error(), "session id") {
			logging.Info("SWu tunnel stop failed", "device", deviceID, "error", err)
		}
		return nil
	}
	_ = manager.Stop(deviceID)
	return nil
}

func (adapter *swuTunnelAdapter) beginConnect(cancel context.CancelFunc) error {
	adapter.lifecycleMu.Lock()
	defer adapter.lifecycleMu.Unlock()
	if adapter.closed {
		return errors.New("runtimehost: SWu tunnel adapter is shut down")
	}
	adapter.connectCancel = cancel
	return nil
}

func (adapter *swuTunnelAdapter) finishConnect(cancel context.CancelFunc) {
	cancel()
	adapter.lifecycleMu.Lock()
	adapter.connectCancel = nil
	adapter.lifecycleMu.Unlock()
}

func (adapter *swuTunnelAdapter) startSession(ctx context.Context) (*swu.Session, error) {
	adapter.lifecycleMu.Lock()
	defer adapter.lifecycleMu.Unlock()
	if adapter.closed {
		return nil, errors.New("runtimehost: SWu tunnel adapter is shut down")
	}
	session, err := adapter.manager.Start(ctx, adapter.deviceID, adapter.config)
	if err != nil {
		return nil, err
	}
	adapter.Session = session
	adapter.activeSession = session
	adapter.stopWait = nil // the previous failed attempt has been waited for
	return session, nil
}

func (adapter *swuTunnelAdapter) stopSession(session *swu.Session) {
	adapter.lifecycleMu.Lock()
	if adapter.activeSession != session {
		adapter.lifecycleMu.Unlock()
		return
	}
	adapter.activeSession = nil
	// A failed attempt is retried with a fresh session: wait until the old one
	// has stopped using the SIM. A Shutdown meanwhile sees the same wait.
	stop := newPendingStop()
	adapter.stopWait = stop
	adapter.lifecycleMu.Unlock()
	stop.issue(adapter.manager, adapter.deviceID)
	if err := stop.Wait(context.Background()); err != nil {
		logging.Info("SWu failed session cleanup reported an error", "device", adapter.deviceID, "error", err)
	}
}

func (adapter *swuTunnelAdapter) UpdateAddresses(oldIP, newIP net.IP) error {
	return adapter.Session.UpdateAddresses(oldIP, newIP)
}
