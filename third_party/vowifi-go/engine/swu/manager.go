package swu

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/iniwex5/vowifi-go/engine/logger"
	"go.uber.org/zap"
)

const DefaultSessionSlot = ""

type managedSession struct {
	session *Session
	cancel  context.CancelFunc
	// runDone is closed when the session goroutine returns; nil for sessions
	// registered without one.
	runDone chan struct{}
}

// ErrSessionStillRunning reports that a stopped session's goroutine has not
// returned yet and may still use the SIM or the network.
var ErrSessionStillRunning = errors.New("swu: session goroutine has not exited")

// SessionManager owns sessions and cancellation functions by device ID and
// optional slot. The empty slot is the default IKE session for a device.
type SessionManager struct {
	mu      sync.Mutex
	devices map[string]map[string]*managedSession
}

func NewSessionManager() *SessionManager {
	return &SessionManager{devices: make(map[string]map[string]*managedSession)}
}

// Start restores the original context/config session constructor.
func (m *SessionManager) Start(
	ctx context.Context,
	deviceID string,
	config *Config,
) (*Session, error) {
	return m.StartSlot(ctx, deviceID, DefaultSessionSlot, config)
}

// StartSlot starts another SWu session for the same device. Overlapping IKE
// reauthentication and a second PDN both use a non-default slot so the old
// default session can keep forwarding.
func (m *SessionManager) StartSlot(
	ctx context.Context,
	deviceID string,
	slot string,
	config *Config,
) (*Session, error) {
	if deviceID == "" {
		return nil, errors.New("session id 不能为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	slot = normalizeSessionSlot(slot)
	session := NewSession(config, logger.With(zap.String("device", deviceID), zap.String("slot", slot)))
	sessionContext, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	slots := m.devices[deviceID]
	if slots == nil {
		slots = make(map[string]*managedSession)
		m.devices[deviceID] = slots
	}
	if _, exists := slots[slot]; exists {
		m.mu.Unlock()
		cancel()
		return nil, errors.New("session id 已存在")
	}
	entry := &managedSession{session: session, cancel: cancel, runDone: make(chan struct{})}
	slots[slot] = entry
	m.mu.Unlock()
	go func() {
		defer close(entry.runDone)
		m.run(sessionContext, session)
	}()
	return session, nil
}

func (m *SessionManager) run(ctx context.Context, session *Session) {
	if err := session.Connect(ctx); err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			session.Logger.Error("SWu managed session exited", zap.Error(err))
		}
		return
	}
	select {
	case <-ctx.Done():
		session.Shutdown()
	case <-session.done:
	}
}

// Stop removes and closes every session for the device.
func (m *SessionManager) Stop(deviceID string) error {
	_, err := m.stopDevice(deviceID)
	return err
}

// StopAndWait is StopAsync followed by waiting with ctx.
func (m *SessionManager) StopAndWait(ctx context.Context, deviceID string) error {
	wait, err := m.StopAsync(deviceID)
	if err != nil {
		return err
	}
	return wait(ctx)
}

// StopAsync removes and starts stopping every session for the device. The
// returned wait blocks until each session goroutine has returned and local
// cleanup has finished, and reports local cleanup failures.
func (m *SessionManager) StopAsync(deviceID string) (func(context.Context) error, error) {
	slots, err := m.stopDevice(deviceID)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) error { return waitStopped(ctx, slots) }, nil
}

// waitStopped waits for the session goroutine and for session.done: when
// another goroutine is already shutting a session down, Shutdown returns
// early and only done marks the end of local cleanup.
func waitStopped(ctx context.Context, slots map[string]*managedSession) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var cleanupErr error
	for _, entry := range slots {
		if entry == nil {
			continue
		}
		var sessionDone <-chan struct{}
		if entry.session != nil {
			sessionDone = entry.session.done
		}
		for _, done := range []<-chan struct{}{entry.runDone, sessionDone} {
			if done == nil {
				continue
			}
			select {
			case <-done:
			case <-ctx.Done():
				return fmt.Errorf("%w: %w", ErrSessionStillRunning, ctx.Err())
			}
		}
		if entry.session != nil {
			cleanupErr = errors.Join(cleanupErr, entry.session.CleanupError())
		}
	}
	return cleanupErr
}

func (m *SessionManager) stopDevice(deviceID string) (map[string]*managedSession, error) {
	m.mu.Lock()
	slots, exists := m.devices[deviceID]
	if exists {
		delete(m.devices, deviceID)
	}
	m.mu.Unlock()
	if !exists || len(slots) == 0 {
		return nil, errors.New("session id 不存在")
	}
	stopManagedSessions(slots)
	return slots, nil
}

// StopSlot removes one named session. Other sessions for the device stay up.
func (m *SessionManager) StopSlot(deviceID, slot string) error {
	slot = normalizeSessionSlot(slot)
	m.mu.Lock()
	slots := m.devices[deviceID]
	entry, exists := slots[slot]
	if exists {
		delete(slots, slot)
		if len(slots) == 0 {
			delete(m.devices, deviceID)
		}
	}
	m.mu.Unlock()
	if !exists {
		return errors.New("session id 不存在")
	}
	stopManagedSession(entry)
	return waitStopped(context.Background(), map[string]*managedSession{slot: entry})
}

// SwapDefault promotes slot to the default session and returns the retired
// default without stopping the promoted session. The caller deletes the
// retired IKE SA after the successor is forwarding.
func (m *SessionManager) SwapDefault(deviceID, slot string) (*Session, error) {
	slot = normalizeSessionSlot(slot)
	if slot == DefaultSessionSlot {
		session, ok := m.Get(deviceID)
		if !ok {
			return nil, errors.New("session id 不存在")
		}
		return session, nil
	}
	m.mu.Lock()
	slots := m.devices[deviceID]
	next, exists := slots[slot]
	if !exists || next == nil {
		m.mu.Unlock()
		return nil, errors.New("session id 不存在")
	}
	retired := slots[DefaultSessionSlot]
	delete(slots, slot)
	slots[DefaultSessionSlot] = next
	m.mu.Unlock()
	if retired == nil {
		return nil, nil
	}
	return retired.session, nil
}

// Get restores the original session-plus-presence result for the default slot.
func (m *SessionManager) Get(deviceID string) (*Session, bool) {
	return m.GetSlot(deviceID, DefaultSessionSlot)
}

// GetSlot returns a named session.
func (m *SessionManager) GetSlot(deviceID, slot string) (*Session, bool) {
	slot = normalizeSessionSlot(slot)
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, exists := m.devices[deviceID][slot]
	if !exists || entry == nil {
		return nil, false
	}
	return entry.session, true
}

// Sessions returns every live session for the device, default slot first.
func (m *SessionManager) Sessions(deviceID string) []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	slots := m.devices[deviceID]
	if len(slots) == 0 {
		return nil
	}
	out := make([]*Session, 0, len(slots))
	if entry := slots[DefaultSessionSlot]; entry != nil && entry.session != nil {
		out = append(out, entry.session)
	}
	for slot, entry := range slots {
		if slot == DefaultSessionSlot || entry == nil || entry.session == nil {
			continue
		}
		out = append(out, entry.session)
	}
	return out
}

// Register retains the additive direct-session registry behavior.
func (m *SessionManager) Register(deviceID string, session *Session) {
	m.mu.Lock()
	slots := m.devices[deviceID]
	if slots == nil {
		slots = make(map[string]*managedSession)
		m.devices[deviceID] = slots
	}
	slots[DefaultSessionSlot] = &managedSession{session: session}
	m.mu.Unlock()
}

// Lookup retains the additive nil-on-missing lookup behavior.
func (m *SessionManager) Lookup(deviceID string) *Session {
	session, _ := m.Get(deviceID)
	return session
}

// Unregister retains the additive remove-and-shutdown behavior.
func (m *SessionManager) Unregister(deviceID string) {
	m.mu.Lock()
	slots, exists := m.devices[deviceID]
	if exists {
		delete(m.devices, deviceID)
	}
	m.mu.Unlock()
	if exists {
		stopManagedSessions(slots)
	}
}

func normalizeSessionSlot(slot string) string {
	return strings.TrimSpace(slot)
}

func stopManagedSessions(slots map[string]*managedSession) {
	for _, entry := range slots {
		stopManagedSession(entry)
	}
}

func stopManagedSession(entry *managedSession) {
	if entry == nil {
		return
	}
	if entry.cancel != nil {
		entry.cancel()
	}
	if entry.session != nil {
		entry.session.Shutdown()
	}
}
