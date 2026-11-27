package vowifihost

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost"
	"github.com/yibaiba/hideck/pkg/logger"
)

type TeardownOptions struct {
	Reason         string
	RestoreSMS     bool
	RestoreSMSMode func(deviceID string)
	SkipInvalidate bool
}

// teardownStopTimeout bounds the wait for the old runtime once this command
// holds the lifecycle slot; the queue wait before it is the caller's.
const teardownStopTimeout = 10 * time.Second

func (m *Manager) StopInstanceForTeardown(ctx context.Context, deviceID, reason string) bool {
	found, _ := m.stopInstanceForTeardown(ctx, deviceID, reason)
	return found
}

// IsOldTaskRunning reports that the old VoWiFi task may still own the SIM and
// modem, either because its runtime has not exited or because the command
// waiting for that exit is still running.
func IsOldTaskRunning(err error) bool {
	return errors.Is(err, runtimehost.ErrRunnerNotExited) || errors.Is(err, ErrPreviousCommandRunning)
}

// IsCleanupOnlyError reports a teardown whose old runtime has exited but
// failed to release a local resource: the modem may be handed over, and the
// failure is still reported to the caller.
func IsCleanupOnlyError(err error) bool {
	return errors.Is(err, runtimehost.ErrCleanupFailed) && !errors.Is(err, runtimehost.ErrRunnerNotExited)
}

// stopInstanceForTeardown reports whether an instance existed. With
// runtimehost.ErrRunnerNotExited the old runtime still owns the SIM and modem:
// the instance stays registered until it exits and is then removed; SMS mode
// and radio are left as they are, a later disable restores them. The caller
// must not touch the SIM, SMS mode or radio.
func (m *Manager) stopInstanceForTeardown(ctx context.Context, deviceID, reason string) (bool, error) {
	if m == nil {
		return false, nil
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return false, nil
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "teardown"
	}
	inst := m.RuntimeStore().Instance(deviceID)
	if inst == nil {
		return false, nil
	}

	if ctx == nil {
		ctx = context.Background()
	}
	stopCtx, cancel := context.WithTimeout(ctx, teardownStopTimeout)
	err := inst.Stop(stopCtx)
	cancel()
	if errors.Is(err, runtimehost.ErrRunnerNotExited) {
		logger.Error("VoWiFi 旧实例未确认退出，保留实例，暂不交接硬件", "device", deviceID, "reason", reason, "err", err)
		m.BroadcastState(deviceID)
		go m.releaseInstanceAfterExit(deviceID, inst, reason)
		return true, fmt.Errorf("设备 %s 的旧 VoWiFi 任务未确认退出(%s): %w", deviceID, reason, err)
	}
	m.RuntimeStore().DeleteInstance(deviceID, inst)
	m.BroadcastState(deviceID)
	if errors.Is(err, runtimehost.ErrCleanupFailed) {
		logger.Error("VoWiFi 旧任务已退出，但本地清理失败", "device", deviceID, "reason", reason, "err", err)
		return true, fmt.Errorf("设备 %s 的旧 VoWiFi 任务已退出，但本地清理失败(%s): %w", deviceID, reason, err)
	}
	if err != nil {
		logger.Warn("VoWiFi teardown 失败", "device", deviceID, "reason", reason, "err", err)
	}
	return true, nil
}

// releaseInstanceAfterExit removes an instance whose runtime outlived its
// teardown once the runtime has exited. Another teardown that removed it
// first wins.
func (m *Manager) releaseInstanceAfterExit(deviceID string, inst *runtimehost.Instance, reason string) {
	err := inst.Stop(context.Background())
	stateMu := m.stateLock(deviceID)
	stateMu.Lock()
	removed := m.RuntimeStore().DeleteInstance(deviceID, inst)
	stateMu.Unlock()
	if !removed {
		return
	}
	if errors.Is(err, runtimehost.ErrCleanupFailed) {
		logger.Error("VoWiFi 旧任务已退出，已移除实例，但本地清理失败", "device", deviceID, "reason", reason, "err", err)
	} else {
		logger.Info("VoWiFi 旧任务已退出，已移除实例", "device", deviceID, "reason", reason, "err", err)
	}
	m.BroadcastState(deviceID)
}

func (m *Manager) TeardownSession(ctx context.Context, deviceID string, opts TeardownOptions) bool {
	found, _ := m.teardownSession(ctx, deviceID, opts)
	return found
}

// teardownSession returns runtimehost.ErrRunnerNotExited without touching SMS
// mode, or a cleanup-only error after a completed handover.
func (m *Manager) teardownSession(ctx context.Context, deviceID string, opts TeardownOptions) (bool, error) {
	if m == nil {
		return false, nil
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return false, nil
	}
	reason := strings.TrimSpace(opts.Reason)
	if reason == "" {
		reason = "teardown"
	}
	if !opts.SkipInvalidate {
		m.invalidateRuntimeState(deviceID, reason)
	}
	var restoreSMS func()
	if opts.RestoreSMS {
		restoreSMS = func() {
			if opts.RestoreSMSMode != nil {
				opts.RestoreSMSMode(deviceID)
			} else if adapter := m.hostAdapter(); adapter != nil {
				adapter.RestoreSMSMode(deviceID)
			}
		}
	}
	found, err := m.stopInstanceForTeardown(ctx, deviceID, reason)
	if !found {
		return false, nil
	}
	if errors.Is(err, runtimehost.ErrRunnerNotExited) {
		return true, err
	}
	if restoreSMS != nil {
		restoreSMS()
	}
	return true, err
}

func (m *Manager) TeardownForReconnect(ctx context.Context, deviceID string) bool {
	m.InvalidateRuntime(deviceID, "reconnect")
	found, err := m.teardownSession(ctx, deviceID, TeardownOptions{Reason: "reconnect", RestoreSMS: true, SkipInvalidate: true})
	if err != nil {
		logger.Error("模块掉线，旧 VoWiFi 实例停止未完成", "device", strings.TrimSpace(deviceID), "err", err)
	}
	if !found {
		return false
	}
	if err == nil {
		logger.Info("模块掉线，已拆除旧 VoWiFi 实例", "device", strings.TrimSpace(deviceID))
	}
	return true
}

func (m *Manager) TeardownForSwitch(ctx context.Context, deviceID string) bool {
	found, _ := m.teardownForSwitch(ctx, deviceID)
	return found
}

func (m *Manager) teardownForSwitch(ctx context.Context, deviceID string) (bool, error) {
	m.ClearDesiredRecoverState(deviceID)
	found, err := m.teardownSession(ctx, deviceID, TeardownOptions{Reason: "switch", RestoreSMS: true})
	if !found || err != nil {
		return found, err
	}
	logger.Info("eSIM 切卡前已拆除旧 VoWiFi 实例", "device", strings.TrimSpace(deviceID))
	return true, nil
}

func (m *Manager) InvalidateRuntime(deviceID, reason string) uint64 {
	if m == nil || strings.TrimSpace(deviceID) == "" {
		return 0
	}
	stateMu := m.stateLock(deviceID)
	stateMu.Lock()
	defer stateMu.Unlock()
	m.lifecycleController().Invalidate(deviceID)
	m.ClearDesiredRecoverState(deviceID)
	return m.invalidateRuntimeStateLocked(deviceID, reason)
}

// Internal teardown must not cancel its own lifecycle command.
func (m *Manager) invalidateRuntimeState(deviceID, reason string) uint64 {
	stateMu := m.stateLock(deviceID)
	stateMu.Lock()
	defer stateMu.Unlock()
	return m.invalidateRuntimeStateLocked(deviceID, reason)
}

func (m *Manager) invalidateRuntimeStateLocked(deviceID, reason string) uint64 {
	if m == nil {
		return 0
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return 0
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "invalidate"
	}
	epoch, hadStartupState := m.RuntimeStore().Invalidate(deviceID)
	logger.Debug("VoWiFi runtime epoch invalidated", "device", deviceID, "reason", reason, "epoch", epoch)
	if hadStartupState {
		m.BroadcastState(deviceID)
	}
	return epoch
}
