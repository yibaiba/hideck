// Package epdg manages SWu sessions by device identity.
package epdg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/iniwex5/vowifi-go/engine/swu"
	"go.uber.org/zap"
)

const waitPollInterval = 200 * time.Millisecond

// ErrEstablishmentTimeout means IKE/SWu did not come up before Wait expired.
// Callers may start a fresh session (new SOCKS5 associate) once.
var ErrEstablishmentTimeout = errors.New("等待 ePDG 隧道建立超时")

type Manager struct {
	mgr *swu.SessionManager
}

func New() *Manager {
	zap.ReplaceGlobals(zap.L().WithOptions(zap.AddCallerSkip(-1)))
	return &Manager{mgr: swu.NewSessionManager()}
}

func (m *Manager) Start(
	ctx context.Context,
	deviceID string,
	config *swu.Config,
) (*swu.Session, error) {
	return m.mgr.Start(ctx, deviceID, config)
}

func (m *Manager) StartSlot(
	ctx context.Context,
	deviceID string,
	slot string,
	config *swu.Config,
) (*swu.Session, error) {
	return m.mgr.StartSlot(ctx, deviceID, slot, config)
}

func (m *Manager) Stop(deviceID string) error {
	return m.mgr.Stop(deviceID)
}

func (m *Manager) StopAndWait(ctx context.Context, deviceID string) error {
	return m.mgr.StopAndWait(ctx, deviceID)
}

func (m *Manager) StopAsync(deviceID string) (func(context.Context) error, error) {
	return m.mgr.StopAsync(deviceID)
}

func (m *Manager) StopSlot(deviceID, slot string) error {
	return m.mgr.StopSlot(deviceID, slot)
}

func (m *Manager) SwapDefault(deviceID, slot string) (*swu.Session, error) {
	return m.mgr.SwapDefault(deviceID, slot)
}

func (m *Manager) GetSlot(deviceID, slot string) (*swu.Session, bool) {
	return m.mgr.GetSlot(deviceID, slot)
}

func (m *Manager) Snapshot(deviceID string) (swu.SessionSnapshot, bool) {
	return m.SnapshotSlot(deviceID, swu.DefaultSessionSlot)
}

func (m *Manager) SnapshotSlot(deviceID, slot string) (swu.SessionSnapshot, bool) {
	session, exists := m.mgr.GetSlot(deviceID, slot)
	if !exists || session == nil {
		return swu.SessionSnapshot{}, false
	}
	return session.Snapshot(), true
}

func (m *Manager) Wait(
	ctx context.Context,
	deviceID string,
	timeout time.Duration,
) (swu.SessionSnapshot, error) {
	return m.WaitSlot(ctx, deviceID, swu.DefaultSessionSlot, timeout)
}

func (m *Manager) WaitSlot(
	ctx context.Context,
	deviceID string,
	slot string,
	timeout time.Duration,
) (swu.SessionSnapshot, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(waitPollInterval)
	defer ticker.Stop()
	for {
		if snapshot, done, err := m.waitResultSlot(deviceID, slot); done {
			return snapshot, err
		}
		select {
		case <-ticker.C:
		case <-timer.C:
			return swu.SessionSnapshot{}, ErrEstablishmentTimeout
		case <-ctx.Done():
			return swu.SessionSnapshot{}, ctx.Err()
		}
	}
}

// ShouldRetryFreshTunnel reports whether the first SWu wait died of a
// timeout while the caller is still allowed to start again.
func ShouldRetryFreshTunnel(ctx context.Context, err error) bool {
	return err != nil && ctx != nil && ctx.Err() == nil && errors.Is(err, ErrEstablishmentTimeout)
}

func (m *Manager) waitResult(deviceID string) (swu.SessionSnapshot, bool, error) {
	return m.waitResultSlot(deviceID, swu.DefaultSessionSlot)
}

func (m *Manager) waitResultSlot(deviceID, slot string) (swu.SessionSnapshot, bool, error) {
	session, exists := m.mgr.GetSlot(deviceID, slot)
	if !exists || session == nil {
		return swu.SessionSnapshot{}, false, nil
	}
	snapshot := session.Snapshot()
	if snapshot.Established {
		return snapshot, true, nil
	}
	if err := session.TerminalError(); err != nil {
		return swu.SessionSnapshot{}, true, fmt.Errorf("ePDG 会话失败: %w", err)
	}
	return swu.SessionSnapshot{}, false, nil
}
