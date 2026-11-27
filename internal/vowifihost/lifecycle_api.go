package vowifihost

import (
	"context"
	"strings"
	"time"

	"github.com/yibaiba/hideck/pkg/logger"
)

type LifecycleRecoverRequest struct {
	DeviceID     string
	Reason       string
	OverrideEPDG string
	Generation   uint64
}

func (m *Manager) Enable(ctx context.Context, deviceID string) error {
	return m.SubmitLifecycle(ctx, LifecycleCommand{
		DeviceID: deviceID,
		Kind:     LifecycleCommandEnable,
		Reason:   "enable",
	})
}

func (m *Manager) Disable(ctx context.Context, deviceID, reason string, restoreRadio bool) error {
	deviceID = strings.TrimSpace(deviceID)
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "disable"
	}
	if deviceID != "" {
		m.InvalidateRuntime(deviceID, reason)
	}
	return m.SubmitLifecycle(ctx, LifecycleCommand{
		DeviceID:           deviceID,
		Kind:               LifecycleCommandDisable,
		Reason:             reason,
		RestoreRadio:       restoreRadio,
		RuntimeInvalidated: deviceID != "",
	})
}

func (m *Manager) Restart(ctx context.Context, deviceID string) error {
	return m.RestartWithQueueTimeout(ctx, deviceID, 0)
}

// RestartWithQueueTimeout cancels the device's current command first, as
// Restart does, and gives up waiting for it after queueWait; the new start
// keeps ctx.
func (m *Manager) RestartWithQueueTimeout(ctx context.Context, deviceID string, queueWait time.Duration) error {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID != "" {
		m.InvalidateRuntime(deviceID, "restart")
	}
	return m.SubmitLifecycle(ctx, LifecycleCommand{
		DeviceID:     deviceID,
		Kind:         LifecycleCommandRestart,
		Reason:       "restart",
		QueueTimeout: queueWait,
	})
}

// StopOrDrain stops the device's VoWiFi as Disable does, but does not put an
// idle device through Disable: it cancels any command in flight, waits for
// the queue to drain and disables only if a runtime shows up meanwhile.
// Starting is read before Active, so a start publishing its instance between
// the two reads is still seen. A cleanup-only failure is logged and treated
// as stopped: the old runtime has exited.
func (m *Manager) StopOrDrain(ctx context.Context, deviceID, reason string, restoreRadio bool) error {
	deviceID = strings.TrimSpace(deviceID)
	if !m.Starting(deviceID) && !m.Active(deviceID) {
		m.InvalidateRuntime(deviceID, reason)
		if err := m.WaitLifecycleIdle(ctx, deviceID); err != nil {
			return err
		}
		if !m.Starting(deviceID) && !m.Active(deviceID) {
			return nil
		}
	}
	err := m.Disable(ctx, deviceID, reason, restoreRadio)
	if IsCleanupOnlyError(err) {
		logger.Error("VoWiFi 旧任务已退出但本地清理失败，继续后续操作", "device", deviceID, "reason", reason, "err", err)
		return nil
	}
	return err
}

func (m *Manager) Recover(ctx context.Context, req LifecycleRecoverRequest) error {
	return m.SubmitLifecycle(ctx, LifecycleCommand{
		DeviceID:     req.DeviceID,
		Kind:         LifecycleCommandRecover,
		Reason:       req.Reason,
		OverrideEPDG: req.OverrideEPDG,
		Generation:   req.Generation,
	})
}

func (m *Manager) SwitchBegin(ctx context.Context, deviceID string) error {
	return m.SubmitLifecycle(ctx, LifecycleCommand{
		DeviceID: deviceID,
		Kind:     LifecycleCommandSwitchBegin,
	})
}

func (m *Manager) SwitchEnd(ctx context.Context, deviceID string, restoreRadio bool) error {
	return m.SubmitLifecycle(ctx, LifecycleCommand{
		DeviceID:     deviceID,
		Kind:         LifecycleCommandSwitchEnd,
		RestoreRadio: restoreRadio,
	})
}
