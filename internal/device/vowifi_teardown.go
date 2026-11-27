package device

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yibaiba/hideck/internal/backend"
	"github.com/yibaiba/hideck/internal/db"
	"github.com/yibaiba/hideck/internal/vowifihost"
	"github.com/yibaiba/hideck/pkg/logger"
)

func (p *Pool) stopVoWiFiAppForTeardown(ctx context.Context, deviceID, reason string) bool {
	host := p.voWiFiHost()
	host.EndWiFiCallingHealth(deviceID, reason)
	return host.StopInstanceForTeardown(ctx, deviceID, reason)
}

func (p *Pool) teardownVoWiFiForReconnect(deviceID string) bool {
	return p.voWiFiHost().TeardownForReconnect(p.ctx, deviceID)
}

// StopVoWiFiBeforeReboot cancels the device's VoWiFi start or session and waits
// until the old runner has exited, so the modem reset never races it.
// hadVoWiFi tells the reboot recovery whether to start VoWiFi again.
func (p *Pool) StopVoWiFiBeforeReboot(ctx context.Context, deviceID string) (hadVoWiFi bool, err error) {
	host := p.voWiFiHost()
	// A stopped instance whose runtime is still exiting was turned off before:
	// it must not be restarted after the reboot.
	hadVoWiFi = host.Running(deviceID)
	return hadVoWiFi, host.StopOrDrain(ctx, deviceID, "manual_reboot", false)
}

// voWiFiStopWaitTimeout bounds how long a caller waits for the old VoWiFi task
// before switching, rebooting or removing hardware; the task keeps the modem
// until it really exits, the caller gets an error instead of waiting forever.
var voWiFiStopWaitTimeout = 20 * time.Second

func (p *Pool) voWiFiStopContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(p.Context(), voWiFiStopWaitTimeout)
}

func (p *Pool) RestoreSMSMode(deviceID string) {
	p.restoreSMSModeAfterVoWiFiTeardown(p.GetWorker(deviceID))
}

func (p *Pool) restoreSMSModeAfterVoWiFiTeardown(w *Worker) {
	if w == nil {
		return
	}

	mode := backend.BackendAT
	if w.Backend != nil {
		mode = backend.NormalizeBackendMode(w.Backend.Mode())
	}
	if mode == backend.BackendQMI {
		w.smsMode = smsModeQMI
		if w.Modem != nil {
			w.Modem.SetDisableURCRead(true)
		}
	} else if mode == backend.BackendPCSC {
		w.smsMode = smsModeVoWiFi
	} else {
		if err := resumeWorkerATSMS(w); err != nil {
			logger.Warn("恢复 AT 短信 URC 失败", "device", w.ID, "backend", mode, "err", err)
		}
	}

	logger.Info("短信模式已恢复", "device", w.ID, "sms_mode", w.smsMode.String())
	w.restoreNetworkAfterVoWiFi = false
}

func (p *Pool) RestoreRadioAfterVoWiFi(deviceID string) error {
	p.mu.RLock()
	isRebuilding := p.rebuilding[deviceID]
	p.mu.RUnlock()
	if isRebuilding {
		return fmt.Errorf("设备 %s 仍处于重建流程中", deviceID)
	}
	if p.IsESIMSwitching(deviceID) {
		return fmt.Errorf("设备 %s 正在切卡，暂不允许恢复射频", deviceID)
	}

	w := p.GetWorker(deviceID)
	if w == nil {
		return fmt.Errorf("设备 %s 不存在", deviceID)
	}
	if w.Backend == nil {
		return nil
	}
	if strings.EqualFold(w.Backend.Mode(), backend.BackendPCSC) {
		w.setCellularRadioSuppressed(true)
		return nil
	}
	blocked, guardErr := p.guardLebaraUKRadioRestore(w, "vowifi_teardown")
	if guardErr != nil {
		return guardErr
	}
	if blocked {
		return nil
	}

	w.setCellularRadioSuppressed(shouldSuppressCellularRadio(w.Config))

	if w.Config.AirplaneEnabled {
		logger.Info("策略要求飞行，收尾不恢复射频", "device", deviceID)
		if nc := w.NetworkController(); nc != nil && nc.IsConnected() {
			_ = w.StopNetwork()
		}
		return nil
	}

	// Cellular mode: keep radio online so the card stays camped even without data.
	if w.Config.PhoneMode == "cellular" {
		logger.Info("蜂窝模式收尾：保持射频在线", "device", deviceID)
		if err := w.Backend.SetOperatingMode(p.ctx, backend.ModeOnline); err != nil {
			logger.Warn("蜂窝模式恢复射频失败", "device", deviceID, "err", err)
		}
		if w.QMICore != nil {
			if err := w.QMICore.SetIMSServiceEnabled(p.ctx, true); err != nil {
				logger.Warn("蜂窝模式恢复原生 IMS 失败", "device", deviceID, "err", err)
			}
		}
		// on_demand: ensure data is closed (may already be closed by call flow).
		if w.Config.DataStrategy != "always" {
			if nc := w.NetworkController(); nc != nil && nc.IsConnected() {
				_ = w.StopNetwork()
			}
		}
		return nil
	}

	logger.Info("退出飞行模式恢复射频", "device", deviceID, "backend", w.Backend.Mode())
	if err := w.Backend.SetOperatingMode(p.ctx, backend.ModeOnline); err != nil {
		return err
	}

	logger.Info("等待射频及基带完全启动重获端口控制权...", "device", deviceID)
	if err := p.waitRadioRecoveryReady(deviceID, 5*time.Second); err != nil {
		logger.Warn("等待射频恢复关键路径就绪超时", "device", deviceID, "err", err)
	}
	waitCtx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()
	if err := waitForCondition(waitCtx, 200*time.Millisecond, func() bool {
		return p.GetWorker(deviceID) != nil
	}); err != nil {
		logger.Warn("等待设备恢复健康超时，继续后续流程", "device", deviceID, "err", err)
	}
	return nil
}

func (p *Pool) RequestVoWiFiRecover(deviceID, reason string) error {
	return p.voWiFiHost().Recover(p.ctx, vowifihost.LifecycleRecoverRequest{DeviceID: deviceID, Reason: reason})
}

func (p *Pool) DisableVoWiFi(deviceID ...string) error {
	if len(deviceID) > 0 {
		target := strings.TrimSpace(deviceID[0])
		if target != "" {
			if p.IsESIMSwitching(target) {
				return fmt.Errorf("设备 %s 正在切卡，暂不允许停用 VoWiFi", target)
			}
			p.stopNativeVoLTE(target, "disable_vowifi")
			if err := p.stopModemVoice(target); err != nil {
				return err
			}
			return p.disableVoWiFiForDevice(target, "disable")
		}
	}

	// Every device gets its stop request; failures are reported together.
	var disableErr error
	for _, devID := range p.voWiFiHost().InstanceIDs() {
		disableErr = errors.Join(disableErr, p.disableVoWiFiForDevice(devID, "disable_all"))
	}
	return disableErr
}

// disableVoWiFiForDevice disables one device within voWiFiStopWaitTimeout. A
// cleanup-only failure still finishes the handover steps and is reported.
func (p *Pool) disableVoWiFiForDevice(deviceID, reason string) error {
	stopCtx, cancel := p.voWiFiStopContext()
	err := p.voWiFiHost().Disable(stopCtx, deviceID, reason, false)
	cancel()
	if err != nil && !vowifihost.IsCleanupOnlyError(err) {
		return err
	}
	p.ClearVoWiFiMWI(deviceID)
	p.applyCardPolicyAfterVoWiFiDisable(deviceID, "vowifi_disabled")
	return err
}

func (p *Pool) applyCardPolicyAfterVoWiFiDisable(deviceID, reason string) {
	if p == nil {
		return
	}
	w := p.GetWorker(deviceID)
	if w == nil {
		return
	}
	p.resolveAndApplyPolicy(w, reason)
}

func (p *Pool) RestartVoWiFi(deviceID string) error {
	// As in main, Restart cancels the current command first; only its wait for
	// that command is bounded, the new start keeps p.ctx.
	return p.voWiFiHost().RestartWithQueueTimeout(p.ctx, deviceID, voWiFiStopWaitTimeout)
}

// RedetectMWIForICCID clears the card's local MWI backoff, then has every
// running runtime that holds the card re-check MWI: reconnect restarts it,
// otherwise it is woken. A device without a running runtime is not started;
// its next normal start reads the reset state (hideck#102).
func (p *Pool) RedetectMWIForICCID(iccid string, reconnect bool) error {
	if p == nil {
		return nil
	}
	iccid = db.CanonicalICCID(iccid)
	if iccid == "" {
		return nil
	}
	host := p.voWiFiHost()
	host.ResetMWISubscriptionBackoff(func(value string) bool { return db.CanonicalICCID(value) == iccid })
	running := 0
	for _, w := range p.GetAllWorkers() {
		if w == nil || db.CanonicalICCID(w.CurrentICCID()) != iccid || !host.Active(w.ID) {
			continue
		}
		running++
		// Active is checked before Restart without holding the
		// lifecycle; a runtime that stops in between is started again.
		if reconnect && w.Config.VoWiFiEnabled && !UsesModemPhoneControl(w.Config.PhoneMode) {
			if err := p.RestartVoWiFi(w.ID); err != nil {
				return err
			}
			continue
		}
		host.WakeMWISubscription(w.ID)
	}
	if running == 0 {
		logger.Info("MWI 重新检测：这张卡没有运行中的 VoWiFi，只重置状态", "reconnect", reconnect)
	}
	return nil
}

func (p *Pool) RestartVoWiFiForICCID(iccid string) error {
	if p == nil {
		return nil
	}
	iccid = db.CanonicalICCID(iccid)
	if iccid == "" {
		return nil
	}
	var deviceIDs []string
	for _, w := range p.GetAllWorkers() {
		if w == nil || !w.Config.VoWiFiEnabled || UsesModemPhoneControl(w.Config.PhoneMode) {
			continue
		}
		if db.CanonicalICCID(w.CurrentICCID()) != iccid {
			continue
		}
		deviceIDs = append(deviceIDs, w.ID)
	}
	for _, deviceID := range deviceIDs {
		if err := p.RestartVoWiFi(deviceID); err != nil {
			return err
		}
	}
	return nil
}
