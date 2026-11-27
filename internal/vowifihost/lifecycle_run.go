package vowifihost

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/engine/swu"
	"github.com/iniwex5/vowifi-go/runtimehost"
	"github.com/yibaiba/hideck/pkg/logger"
)

const lifecycleReadyTimeout = 3 * time.Second

type runtimeEnableRequest struct {
	DeviceID     string
	AllowSwitch  bool
	OverrideEPDG string
	Generation   uint64
	Reason       string
}

func (m *Manager) runLifecycleCommand(ctx context.Context, cmd LifecycleCommand) error {
	if m == nil {
		return fmt.Errorf("vowifi host manager is nil")
	}
	switch cmd.Kind {
	case LifecycleCommandEnable:
		return m.enableRuntime(ctx, runtimeEnableRequest{
			DeviceID:     cmd.DeviceID,
			AllowSwitch:  cmd.AllowSwitch,
			OverrideEPDG: cmd.OverrideEPDG,
			Generation:   cmd.Generation,
			Reason:       cmd.Reason,
		})
	case LifecycleCommandDisable:
		healthTargets := m.lifecycleTargetIDs(cmd.DeviceID)
		if m.hostAdapter() == nil {
			return m.disableRuntime(ctx, cmd.DeviceID, cmd.Reason, cmd.RestoreRadio, cmd.RuntimeInvalidated)
		}
		for _, deviceID := range healthTargets {
			m.EndWiFiCallingHealth(deviceID, cmd.Reason)
		}
		return m.disableRuntime(ctx, cmd.DeviceID, cmd.Reason, cmd.RestoreRadio, cmd.RuntimeInvalidated)
	case LifecycleCommandRestart:
		return m.restartRuntime(ctx, cmd.DeviceID, cmd.Generation)
	case LifecycleCommandRecover:
		return m.recoverRuntime(ctx, cmd.DeviceID, cmd.Reason, cmd.Generation, cmd.OverrideEPDG)
	case LifecycleCommandSwitchBegin:
		m.EndWiFiCallingHealth(cmd.DeviceID, "switch")
		_, err := m.teardownForSwitch(ctx, cmd.DeviceID)
		return err
	case LifecycleCommandSwitchEnd:
		if cmd.RestoreRadio {
			return m.enableRuntime(ctx, runtimeEnableRequest{
				DeviceID:    cmd.DeviceID,
				AllowSwitch: true,
				Generation:  cmd.Generation,
				Reason:      "switch_restore",
			})
		}
		return nil
	default:
		return fmt.Errorf("unsupported vowifi lifecycle command kind: %d", int(cmd.Kind))
	}
}

func (m *Manager) lifecycleTargetIDs(deviceID string) []string {
	if deviceID = strings.TrimSpace(deviceID); deviceID != "" {
		return []string{deviceID}
	}
	return m.InstanceIDs()
}

func (m *Manager) enableRuntime(ctx context.Context, req runtimeEnableRequest) (retErr error) {
	adapter := m.hostAdapter()
	if adapter == nil {
		return fmt.Errorf("vowifi host adapter is not configured")
	}
	deviceID := strings.TrimSpace(req.DeviceID)
	if deviceID == "" {
		return fmt.Errorf("vowifi enable device_id is empty")
	}
	if !req.AllowSwitch && adapter.IsSwitching(deviceID) {
		return fmt.Errorf("设备 %s 正在切卡，暂不允许启动 VoWiFi", deviceID)
	}

	traceID := runtimehost.NewTraceID()
	baseCtx := hostContext(ctx, adapter)
	startCtx := runtimehost.WithTraceID(baseCtx, traceID)
	startedAt := time.Now()

	startClaim, err := m.beginRuntimeStart(startCtx, deviceID)
	if err != nil {
		return err
	}
	if startClaim.Active {
		// Stopped never turns back, so checking after the claim also covers a
		// Stop that raced it.
		if inst := m.RuntimeStore().Instance(deviceID); inst != nil && inst.Stopped() {
			return fmt.Errorf("设备 %s 的旧 VoWiFi 任务仍在退出，暂不能启动", deviceID)
		}
		return nil
	}
	if startClaim.Starting {
		return fmt.Errorf("设备 %s 的 VoWiFi 正在启动中", deviceID)
	}
	if !startClaim.Accepted {
		return fmt.Errorf("设备 %s 的 VoWiFi 启动声明失败", deviceID)
	}
	startupEpoch := startClaim.Epoch
	startFinalized := false
	defer func() {
		if !startFinalized {
			m.FailStart(deviceID, startupEpoch, runtimehost.State{}, retErr)
		}
	}()

	preparedStart, err := m.PrepareStart(startCtx, deviceID, traceID, req.OverrideEPDG)
	if startCtx.Err() != nil || !m.ShouldRun(deviceID, startupEpoch) {
		return context.Canceled
	}
	if err != nil {
		return err
	}
	generation := m.ensureLifecycleGeneration(deviceID, req.Generation, req.Reason)
	modemIface := preparedStart.Modem
	if modemIface == nil {
		return fmt.Errorf("设备 %s 的 VoWiFi modem adapter 未准备", deviceID)
	}
	initialState := preparedStart.StartupState
	m.RecordStartupStateForEpoch(deviceID, startupEpoch, initialState)
	stateMu := m.stateLock(deviceID)
	stateMu.Lock()
	if startCtx.Err() != nil || !m.ShouldRun(deviceID, startupEpoch) {
		stateMu.Unlock()
		return context.Canceled
	}
	beforeStart := m.BeforeStart(deviceID, modemIface, preparedStart.Proxy)
	stateMu.Unlock()

	result, err := m.StartRuntime(startCtx, RuntimeStartRequest{
		DeviceID:      deviceID,
		TraceID:       traceID,
		Epoch:         startupEpoch,
		StartedAt:     startedAt,
		Prepared:      preparedStart,
		Modem:         modemIface,
		VoiceGateway:  m.voiceGateway,
		Dataplane:     runtimehost.DataplanePolicy{Mode: swu.DataplaneModeUserspace},
		DeliveryStore: m.deliveryStore,
		Dispatch:      m.dispatcher,
		BeforeStart:   beforeStart,
		TunnelFactory: preparedStart.TunnelFactory,
	})
	if err != nil {
		if startCtx.Err() != nil || !m.ShouldRun(deviceID, startupEpoch) {
			if errors.Is(err, runtimehost.ErrCleanupFailed) {
				logger.Error("取消的 VoWiFi 启动已退出，但本地清理失败", "trace_id", traceID, "device", deviceID, "err", err)
			} else {
				logger.Info("取消的 VoWiFi 启动已退出", "trace_id", traceID, "device", deviceID, "cost_ms", time.Since(startedAt).Milliseconds())
			}
			return context.Canceled
		}
		state, ok := m.State(deviceID)
		if !ok {
			state = initialState
		}
		return adapter.HandleStartupError(StartupErrorRequest{
			TraceID:             traceID,
			DeviceID:            deviceID,
			RuntimeEPDGOverride: strings.TrimSpace(req.OverrideEPDG),
			Generation:          generation,
			StartedAt:           startedAt,
			State:               state,
			Err:                 err,
		})
	}

	if result.Stale {
		startFinalized = true
		logger.Info("VoWiFi 启动完成但运行态已被更新取代，已关闭该旧实例",
			"trace_id", traceID,
			"device", deviceID,
			"generation", generation,
			"cost_ms", time.Since(startedAt).Milliseconds())
		return nil
	}

	startFinalized = true
	logger.Info("VoWiFi SWu 隧道已建立，正在等待 IMS 注册",
		"trace_id", traceID, "device", deviceID, "cost_ms", time.Since(startedAt).Milliseconds())
	logger.Debug("EnableVoWiFi 已进入后台 IMS 注册阶段",
		"trace_id", traceID, "device", deviceID, "cost_ms", time.Since(startedAt).Milliseconds())
	return nil
}

func (m *Manager) disableRuntime(ctx context.Context, reasonDeviceID, reason string, restoreRadio, skipInvalidate bool) error {
	adapter := m.hostAdapter()
	if adapter == nil {
		return fmt.Errorf("vowifi host adapter is not configured")
	}
	if ctx == nil {
		ctx = hostContext(nil, adapter)
	}
	if strings.TrimSpace(reason) == "" {
		reason = "disable"
	}

	var devIDs []string
	if target := strings.TrimSpace(reasonDeviceID); target != "" {
		devIDs = []string{target}
	} else {
		devIDs = m.InstanceIDs()
	}

	var disableErr error
	for _, devID := range devIDs {
		stopped, err := m.teardownSession(ctx, devID, TeardownOptions{
			Reason:         reason,
			RestoreSMS:     true,
			SkipInvalidate: skipInvalidate,
		})
		if err != nil {
			disableErr = errors.Join(disableErr, err)
			if !IsCleanupOnlyError(err) {
				// The old runner may still own the SIM and modem: leave SMS mode
				// and radio untouched; a later disable restores them.
				continue
			}
		}
		if !stopped {
			logger.Info("VoWiFi 实例不存在，执行强制恢复流程", "device", devID, "reason", reason, "restore_radio", restoreRadio)
			adapter.RestoreSMSMode(devID)
		}
		if restoreRadio {
			if err := adapter.RestoreRadioAfterVoWiFi(devID); err != nil {
				logger.Warn("VoWiFi 停用后恢复射频失败", "device", devID, "reason", reason, "err", err)
			}
		}
		logger.Info("VoWiFi 已禁用、QMI/AT 短信轮询已恢复，当前未自动恢复射频/数据", "device", devID, "reason", reason)
	}
	return disableErr
}

func (m *Manager) restartRuntime(ctx context.Context, deviceID string, generation uint64) error {
	adapter := m.hostAdapter()
	if adapter == nil {
		return fmt.Errorf("vowifi host adapter is not configured")
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return fmt.Errorf("vowifi restart device_id is empty")
	}
	if !adapter.WorkerExists(deviceID) {
		return fmt.Errorf("设备 %s 不存在，无法重启 VoWiFi", deviceID)
	}

	logger.Info("准备重启 VoWiFi 隧道...", "device", deviceID)
	if err := m.disableRuntime(ctx, deviceID, "restart", false, false); err != nil {
		if !IsCleanupOnlyError(err) {
			return fmt.Errorf("重启 VoWiFi 失败，旧任务未停止: %w", err)
		}
		logger.Error("重启 VoWiFi：旧任务已退出但本地清理失败，继续启动", "device", deviceID, "err", err)
	}
	if err := m.enableWhenReady(ctx, deviceID, lifecycleReadyTimeout, "restart", "", generation); err != nil {
		return fmt.Errorf("重启 VoWiFi 失败: %w", err)
	}
	logger.Info("VoWiFi 重启流程执行完成", "device", deviceID)
	return nil
}

func (m *Manager) recoverRuntime(ctx context.Context, deviceID, reason string, generation uint64, overrideEPDG string) error {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return fmt.Errorf("vowifi recover device_id is empty")
	}
	if strings.TrimSpace(reason) == "" {
		reason = "recover"
	}
	if inst := m.RuntimeStore().Instance(deviceID); inst != nil && inst.Stopped() {
		return fmt.Errorf("恢复 VoWiFi 失败(%s)：设备 %s 的旧任务仍在退出", reason, deviceID)
	}
	if m.Active(deviceID) || m.Starting(deviceID) {
		logger.Debug("VoWiFi recover 跳过：运行态仍活跃或正在启动", "device", deviceID, "reason", reason)
		return nil
	}
	if _, err := m.teardownSession(ctx, deviceID, TeardownOptions{Reason: reason, RestoreSMS: true}); err != nil {
		if !IsCleanupOnlyError(err) {
			return fmt.Errorf("恢复 VoWiFi 失败(%s)，旧任务未停止: %w", reason, err)
		}
		logger.Error("恢复 VoWiFi：旧任务已退出但本地清理失败，继续启动", "device", deviceID, "reason", reason, "err", err)
	}
	if err := m.enableWhenReady(ctx, deviceID, lifecycleReadyTimeout, reason, overrideEPDG, generation); err != nil {
		return fmt.Errorf("恢复 VoWiFi 失败(%s): %w", reason, err)
	}
	return nil
}

func (m *Manager) enableWhenReady(ctx context.Context, deviceID string, timeout time.Duration, reason, overrideEPDG string, generation uint64) error {
	adapter := m.hostAdapter()
	if adapter == nil {
		return fmt.Errorf("vowifi host adapter is not configured")
	}
	if generation == 0 {
		logger.Debug("VoWiFi ready-enable received zero generation; runtime fallback may allocate one", "device", strings.TrimSpace(deviceID), "reason", strings.TrimSpace(reason))
	}
	if err := adapter.WaitQMICoreReady(deviceID, timeout); err != nil {
		return fmt.Errorf("等待设备 %s QMI Core 就绪失败(%s): %w", deviceID, reason, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := adapter.WaitWorkerReady(deviceID, timeout); err != nil {
		return fmt.Errorf("等待设备 %s 就绪失败(%s): %w", deviceID, reason, err)
	}
	return m.enableRuntime(ctx, runtimeEnableRequest{
		DeviceID:     deviceID,
		OverrideEPDG: overrideEPDG,
		Generation:   generation,
		Reason:       reason,
	})
}

func (m *Manager) ensureLifecycleGeneration(deviceID string, generation uint64, reason string) uint64 {
	if generation != 0 {
		return generation
	}
	fallback := m.NextLifecycleGeneration(deviceID)
	logger.Debug("VoWiFi 启动缺少 lifecycle generation，已使用 fallback generation", "device", strings.TrimSpace(deviceID), "reason", strings.TrimSpace(reason), "generation", fallback)
	return fallback
}

func hostContext(ctx context.Context, adapter Adapter) context.Context {
	if ctx != nil {
		return ctx
	}
	if adapter != nil {
		if adapterCtx := adapter.Context(); adapterCtx != nil {
			return adapterCtx
		}
	}
	return context.Background()
}
