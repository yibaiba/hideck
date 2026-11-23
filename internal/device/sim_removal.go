package device

import (
	"fmt"
	"strings"
	"time"

	"github.com/yibaiba/hideck/internal/backend"
	"github.com/yibaiba/hideck/internal/db"
	"github.com/yibaiba/hideck/internal/modem"
	"github.com/yibaiba/hideck/pkg/logger"
)

func (w *Worker) markSIMRemoved(reason string, generation uint64) bool {
	now := time.Now()
	w.cacheMu.Lock()
	defer w.cacheMu.Unlock()
	strict := w.atDeviceRules().strictSIMIdentity
	if strict && w.state.Identity.Generation != generation {
		return false
	}
	hadIdentity := strings.TrimSpace(w.state.Identity.ICCID) != "" ||
		strings.TrimSpace(w.state.Identity.IMSI) != "" || w.state.Identity.Ready
	if hadIdentity || (strict && w.state.Identity.LastError != "sim_removed") {
		w.state.Identity.Generation++
	}
	w.state.Identity.ICCID = ""
	w.state.Identity.IMSI = ""
	w.state.Identity.NativeSPN = ""
	w.clearSIMMetadataLocked()
	w.state.Identity.Ready = false
	w.state.Identity.Phase = simIdentityPhaseDegraded
	w.state.Identity.TargetICCID = ""
	w.state.Identity.LastReason = strings.TrimSpace(reason)
	w.state.Identity.LastError = "sim_removed"
	w.state.Runtime.SimInserted = false
	w.state.Meta.IdentityUpdatedAt = now
	w.state.Meta.RuntimeUpdatedAt = now
	w.state.Meta.UpdatedAt = now
	return true
}

func (w *Worker) scheduleSIMStatusCheck(source string, inserted *bool, state string) {
	if w.Pool == nil {
		return
	}
	owner := workerLifecycleOwner{worker: w, generation: w.generation}
	w.Pool.handleSIMStatusEventForOwner(owner, source, inserted, state)
}

func (w *Worker) recordStrictSIMAbsence(generation uint64, source string) {
	w.degradeStrictSIMIdentity(generation, source, errATNoSIM)
	inserted := false
	w.scheduleSIMStatusCheck(source, &inserted, "")
}

func (w *Worker) recordStrictSIMLock(generation uint64, source string) {
	if w.Pool != nil && w.Pool.IsESIMSwitching(w.ID) {
		return
	}
	if w.markSIMPINRequired(generation) {
		w.scheduleSIMStatusCheck(source, nil, "SIM PIN")
	}
}

func (w *Worker) degradeStrictSIMIdentity(generation uint64, reason string, err error) bool {
	now := time.Now()
	w.cacheMu.Lock()
	defer w.cacheMu.Unlock()
	if w.state.Identity.Generation != generation || !w.state.Identity.Ready ||
		w.state.Identity.Phase == simIdentityPhaseTransitioning {
		return false
	}
	w.markSIMIdentityDegradedLocked(reason, err, now)
	w.state.Identity.Generation++
	return true
}

func (w *Worker) markSIMPINRequired(generation uint64) bool {
	now := time.Now()
	w.cacheMu.Lock()
	defer w.cacheMu.Unlock()
	if w.state.Identity.Generation != generation {
		return false
	}
	if w.state.Identity.LastError == modem.ErrSIMPINRequired.Error() && !w.state.Identity.Ready {
		return true
	}
	w.markSIMIdentityDegradedLocked("sim_pin_required", modem.ErrSIMPINRequired, now)
	w.state.Identity.Generation++
	w.state.Runtime.SimInserted = false
	w.state.Meta.RuntimeUpdatedAt = now
	return true
}

// Caller holds the device policy transition lock.
func (p *Pool) handleConfirmedSIMPINRequired(owner workerLifecycleOwner, generation uint64) {
	if !p.workerLifecycleOwnerCurrent(owner) || p.IsESIMSwitching(owner.worker.ID) {
		return
	}
	w := owner.worker
	if !w.markSIMPINRequired(generation) {
		return
	}
	if err := p.retirePhoneRuntimesForSIMChangeLocked(owner); err != nil {
		logger.Warn("SIM PIN 锁定后停止电话业务失败", "device", w.ID, "err", err)
	}
	p.broadcastVoWiFiStateChange(w.ID)
}

func (p *Pool) retirePhoneRuntimesForSIMChange(owner workerLifecycleOwner) error {
	if p == nil || owner.worker == nil {
		return errPolicyOwnerChanged
	}
	deviceID := owner.worker.ID
	transition := p.policyTransitionFor(deviceID)
	transition.Lock()
	defer transition.Unlock()
	return p.retirePhoneRuntimesForSIMChangeLocked(owner)
}

func (p *Pool) retirePhoneRuntimesForSIMChangeLocked(owner workerLifecycleOwner) error {
	if !p.workerLifecycleOwnerCurrent(owner) {
		return errPolicyOwnerChanged
	}
	deviceID := owner.worker.ID
	p.stopNativeVoLTE(deviceID, "sim_identity_changed")
	if err := p.stopModemVoice(deviceID); err != nil {
		return fmt.Errorf("stop modem voice: %w", err)
	}
	if !p.IsVoWiFiActive(deviceID) && !p.voWiFiHost().Starting(deviceID) && p.GetVoWiFiAppForDevice(deviceID) == nil {
		return nil
	}
	if err := p.voWiFiHost().Disable(p.Context(), deviceID, "sim_identity_changed", false); err != nil {
		return fmt.Errorf("stop VoWiFi: %w", err)
	}
	if !p.workerLifecycleOwnerCurrent(owner) {
		return errPolicyOwnerChanged
	}
	return nil
}

func (p *Pool) detachPersistedSIMBinding(worker *Worker) {
	if p == nil || worker == nil || resolvedBackendMode(worker.Config) == backend.BackendPCSC ||
		(worker.Backend != nil && worker.Backend.Mode() == backend.BackendPCSC) {
		return
	}
	status := worker.ProjectDeviceStatus()
	imei := strings.TrimSpace(status.IMEI)
	if imei == "" {
		imei = strings.TrimSpace(worker.Config.ModemIMEI)
	}
	if imei == "" {
		logger.Warn("SIM 拔出后无法解除设备绑定：IMEI 为空", "device", worker.ID)
		return
	}
	if err := db.UpdateDeviceCurrentSIM(imei, nil); err != nil {
		logger.Warn("SIM 拔出后解除设备绑定失败", "device", worker.ID, "imei", imei, "err", err)
	}
}
