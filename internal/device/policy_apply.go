package device

import (
	"errors"
	"time"

	"github.com/yibaiba/hideck/internal/cardpolicy"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/pkg/logger"
)

type policyApplyResult struct {
	Applied bool
	ICCID   string
	Reason  string
	Err     error
}

// resolveAndApplyPolicy 解析 worker 当前 ICCID 的策略，投影并复用现有 apply 路径。
func (p *Pool) resolveAndApplyPolicy(worker *Worker, reason string) policyApplyResult {
	if p == nil || worker == nil || p.policyResolver == nil {
		return policyApplyResult{}
	}
	transition := p.policyTransitionFor(worker.ID)
	transition.Lock()
	if transition.mbnSession.Load() != nil {
		transition.Unlock()
		return policyApplyResult{Reason: "mbn_operation_active"}
	}
	owner := capturePolicyOwner(worker, transition)
	ownerErr := p.validatePolicyOwner(owner, transition)
	transition.Unlock()
	iccid := owner.iccid
	if iccid == "" {
		logger.Info("跳过策略投影：ICCID 未就绪", "device", worker.ID, "reason", reason)
		return policyApplyResult{Reason: "iccid_empty"}
	}
	if ownerErr != nil {
		return policyApplyResult{ICCID: iccid, Reason: "owner_changed", Err: ownerErr}
	}
	releaseOperation, err := p.AcquireDeviceOperation(worker.ID)
	if err != nil {
		return policyApplyResult{ICCID: iccid, Reason: "mbn_operation_active", Err: err}
	}
	defer releaseOperation()
	pol, err := p.policyResolver.Resolve(iccid)
	if err != nil {
		logger.Warn("解析卡策略失败", "device", worker.ID, "iccid", iccid, "err", err)
		return policyApplyResult{ICCID: iccid, Reason: "resolve_failed", Err: err}
	}
	transition.Lock()
	defer transition.Unlock()
	validate := func() error { return p.validatePolicyOwner(owner, transition) }
	if err := validate(); err != nil {
		return policyApplyResult{ICCID: iccid, Reason: "owner_changed", Err: err}
	}
	// Finish the old call before projecting a policy that disallows new calls.
	// Failed hangup leaves the current session and policy available for retry.
	var finishNativeTransition func()
	if !IsNativeVoLTEMode(pol.PhoneMode) || !pol.VoWiFiEnabled || pol.AirplaneEnabled {
		finishNativeTransition = p.beginNativeVoLTETransition(worker.ID)
		defer func() {
			if finishNativeTransition != nil {
				finishNativeTransition()
			}
		}()
	}
	if !IsModemVoiceMode(pol.PhoneMode) || !pol.VoWiFiEnabled || pol.AirplaneEnabled {
		if err := p.stopModemVoice(worker.ID); err != nil {
			return policyApplyResult{ICCID: iccid, Reason: "modem_voice_stop_failed", Err: err}
		}
	}
	modemVoiceHandoff := IsModemVoiceMode(pol.PhoneMode) && pol.VoWiFiEnabled && !pol.AirplaneEnabled
	if err := validate(); err != nil {
		return policyApplyResult{ICCID: iccid, Reason: "owner_changed", Err: err}
	}
	if modemVoiceHandoff {
		if err := p.stopNativeVoLTEForModemVoice(worker.ID); err != nil {
			return policyApplyResult{ICCID: iccid, Reason: "native_volte_stop_failed", Err: err}
		}
	}
	if err := validate(); err != nil {
		return policyApplyResult{ICCID: iccid, Reason: "owner_changed", Err: err}
	}
	if worker.Config.PhoneMode != pol.PhoneMode || !pol.VoWiFiEnabled ||
		(pol.PhoneMode == "cellular" && (pol.AirplaneEnabled || pol.DataStrategy != "always")) {
		// A policy that keeps the radio off must not let the teardown bring it online first.
		// Otherwise the teardown still restores the radio and native IMS.
		if err := p.stopSoftwareIMS(worker.ID, !policyKeepsRadioOff(pol)); err != nil {
			return policyApplyResult{ICCID: iccid, Reason: "software_ims_stop_failed", Err: err}
		}
	}
	if err := validate(); err != nil {
		return policyApplyResult{ICCID: iccid, Reason: "owner_changed", Err: err}
	}
	if err := applyPolicyToWorker(worker, pol); err != nil {
		logger.Warn("投影卡策略失败", "device", worker.ID, "iccid", iccid, "err", err)
		return policyApplyResult{ICCID: iccid, Reason: "apply_failed", Err: err}
	}
	effective := worker.Config
	logger.Info("已投影卡策略", "device", worker.ID, "iccid", iccid,
		"network", effective.NetworkEnabled, "vowifi", effective.VoWiFiEnabled,
		"airplane", effective.AirplaneEnabled, "reason", reason)
	// Apply the current SIM's RF restrictions before inspecting or queuing MBN.
	// Keep the shared operation lease while temporarily releasing the transition
	// lock: inspection needs that lock, and a queued writer must wait for us.
	radioSuppressed := shouldSuppressCellularRadio(effective)
	if radioSuppressed {
		p.enterAirplaneModeFromPolicy(worker, reason)
	}
	nativeEnabled := IsNativeVoLTEMode(effective.PhoneMode) && PhoneServiceEnabled(effective) && !effective.AirplaneEnabled
	if !nativeEnabled && !modemVoiceHandoff {
		p.stopNativeVoLTE(worker.ID, reason)
	}
	var mbnPending bool
	var mbnErr error
	if reason != "mbn_operation_finished" && p.mbnService != nil {
		transition.Unlock()
		mbnPending, mbnErr = p.reconcileMBN(worker)
		transition.Lock()
		if err := validate(); err != nil {
			return policyApplyResult{ICCID: iccid, Reason: "owner_changed", Err: err}
		}
		if worker.Config != effective {
			return policyApplyResult{ICCID: iccid, Reason: "policy_changed", Err: errors.New("MBN 检查期间设备策略已改变，请按当前策略重试")}
		}
		if (mbnPending || mbnErr != nil) && !radioSuppressed {
			return policyApplyResult{ICCID: iccid, Reason: "mbn_policy", Err: mbnErr}
		}
	}

	// 三态分支：VoWiFi / 纯飞行 / 在线(含连网)。射频模式按策略真正切换，
	// 补齐此前“airplane 字段被投影但从不执行”的缺口。
	switch {
	case effective.AirplaneEnabled:
		// 飞行优先：蜂窝软件电话可以保持开启，只关射频和流量。
	case PhoneServiceEnabled(effective) && PhoneModeCampsOnCell(effective.PhoneMode):
		// 蜂窝软件电话 / 原生 VoLTE：射频保持在线以驻网。网络开着才连上网数据。
		p.exitAirplaneModeIfNeeded(worker, reason)
		if err := p.applyNetworkPreference(worker); err != nil {
			logger.Warn("应用网络偏好失败", "device", worker.ID, "err", err)
		}
	case PhoneServiceEnabled(effective):
		// WiFi calling：先关射频，再停数据。不要等 VoWiFi 启动后再补飞。
		p.enterAirplaneModeFromPolicy(worker, reason)
		if err := p.applyNetworkPreference(worker); err != nil {
			logger.Warn("应用网络偏好失败", "device", worker.ID, "err", err)
		}
	default:
		// 在线待机或连网：飞行关着就驻网；网络开关只决定是否拉起数据。
		p.exitAirplaneModeIfNeeded(worker, reason)
		if err := p.applyNetworkPreference(worker); err != nil {
			logger.Warn("应用网络偏好失败", "device", worker.ID, "err", err)
		}
	}
	if err := validate(); err != nil {
		return policyApplyResult{ICCID: iccid, Reason: "owner_changed", Err: err}
	}
	if nativeEnabled {
		// Carrier policy can project a software IMS mode into native VoLTE.
		if finishNativeTransition != nil {
			finishNativeTransition()
			finishNativeTransition = nil
		}
		p.clearDesiredVoWiFiRecoverState(worker.ID)
		p.scheduleNativeVoLTE(worker.ID, reason)
	} else {
		if PhoneServiceEnabled(effective) && !UsesModemPhoneControl(effective.PhoneMode) && !cellularSoftwarePhoneHeld(worker, pol) {
			p.scheduleDesiredVoWiFiRecover(worker.ID, reason, time.Now())
		} else {
			p.clearDesiredVoWiFiRecoverState(worker.ID)
		}
	}
	if err := validate(); err != nil {
		return policyApplyResult{ICCID: iccid, Reason: "owner_changed", Err: err}
	}
	if err := p.reconcileModemVoice(worker); err != nil {
		return policyApplyResult{ICCID: iccid, Reason: "modem_voice_failed", Err: err}
	}
	if mbnPending || mbnErr != nil {
		return policyApplyResult{ICCID: iccid, Reason: "mbn_policy", Err: mbnErr}
	}
	return policyApplyResult{Applied: true, ICCID: iccid, Reason: reason}
}

// policyKeepsRadioOff reports whether projecting pol leaves the cellular radio off.
func policyKeepsRadioOff(pol cardpolicy.Policy) bool {
	return shouldSuppressCellularRadio(config.DeviceConfig{
		AirplaneEnabled: pol.AirplaneEnabled,
		VoWiFiEnabled:   pol.VoWiFiEnabled,
		PhoneMode:       pol.PhoneMode,
	})
}
