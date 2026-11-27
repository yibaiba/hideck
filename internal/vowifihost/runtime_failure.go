package vowifihost

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost"
	"github.com/yibaiba/hideck/pkg/logger"
)

const failedRuntimeStopTimeout = 10 * time.Second

const ikeReauthenticationReason = "ike_reauthentication"

func isTerminalRuntimeFailure(state runtimehost.State) bool {
	return state.SessionState == "error" && strings.TrimSpace(state.LastError) != ""
}

func (m *Manager) releaseFailedRuntime(deviceID string, inst *runtimehost.Instance, state runtimehost.State) bool {
	controlledReauth := state.LastErrorClass == runtimehost.ErrorClassReauthentication
	if m == nil || inst == nil {
		return false
	}
	invalidationReason := "runtime_failure"
	if controlledReauth {
		invalidationReason = ikeReauthenticationReason
	}
	stateMu := m.stateLock(deviceID)
	stateMu.Lock()
	// Claim once: the first terminal event or teardown to stop the instance
	// owns its release; later events see Stopped and leave it alone.
	if m.RuntimeStore().Instance(deviceID) != inst || inst.Stopped() {
		stateMu.Unlock()
		return false
	}
	claimCtx, claimed := context.WithCancel(context.Background())
	claimed()
	_ = inst.Stop(claimCtx) // starts the release task without waiting
	m.lifecycleController().Invalidate(deviceID)
	m.ClearDesiredRecoverState(deviceID)
	epoch := m.invalidateRuntimeStateLocked(deviceID, invalidationReason)
	stateMu.Unlock()
	m.BroadcastState(deviceID)

	// Keep the instance registered until its runtime has exited, so teardowns
	// arriving meanwhile wait for it instead of finding nothing to stop.
	stopCtx, cancel := context.WithTimeout(context.Background(), failedRuntimeStopTimeout)
	err := inst.Stop(stopCtx)
	cancel()
	if errors.Is(err, runtimehost.ErrRunnerNotExited) {
		logger.Error("VoWiFi 故障实例未在期限内退出，保留实例直到退出", "device", deviceID, "err", err)
		err = inst.Stop(context.Background())
	}
	if err != nil {
		logger.Warn("VoWiFi 故障实例停止失败", "device", deviceID, "err", err)
	}
	stateMu.Lock()
	if !m.RuntimeStore().DeleteInstance(deviceID, inst) {
		// A teardown that waited for the same exit already handed over.
		stateMu.Unlock()
		return true
	}
	m.BroadcastState(deviceID)
	if !m.ShouldRun(deviceID, epoch) || m.Active(deviceID) || m.Starting(deviceID) {
		stateMu.Unlock()
		return true
	}
	if adapter := m.hostAdapter(); adapter != nil {
		adapter.RestoreSMSMode(deviceID)
	}
	stateMu.Unlock()
	if controlledReauth {
		logger.Info("VoWiFi IKE 重鉴权旧实例已释放，立即请求新运行时", "device", deviceID)
		m.requestRuntimeRecycle(deviceID, ikeReauthenticationReason)
		return true
	}
	logger.Error("VoWiFi 运行时故障已释放，等待目标态恢复",
		"device", deviceID,
		"class", state.LastErrorClass,
		"reason", state.LastReason,
		"err", state.LastError)
	return true
}
