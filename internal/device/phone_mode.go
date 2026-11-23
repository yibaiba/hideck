package device

import (
	"errors"
	"strings"

	"github.com/iniwex5/vowifi-go/runtimehost/carrier"
	"github.com/yibaiba/hideck/internal/cardpolicy"
	"github.com/yibaiba/hideck/internal/config"
)

var ErrModemVoiceSoftwareIMS = errors.New("模组直拨模式不使用软件 IMS")

type PhoneModeUnsupportedError struct {
	Reason string
}

func (e *PhoneModeUnsupportedError) Error() string {
	return e.Reason
}

const (
	PhoneModeWiFi       = "wifi"
	PhoneModeCellular   = "cellular"
	PhoneModeVoLTE      = "volte"
	PhoneModeModemVoice = "modem_voice"
)

func IsModemVoiceMode(mode string) bool {
	return strings.TrimSpace(mode) == PhoneModeModemVoice
}

// UsesModemPhoneControl excludes modem-controlled modes from software IMS.
func UsesModemPhoneControl(mode string) bool {
	return IsNativeVoLTEMode(mode) || IsModemVoiceMode(mode)
}

func IsNativeVoLTEMode(mode string) bool {
	return strings.TrimSpace(mode) == PhoneModeVoLTE
}

func PhoneModeCampsOnCell(mode string) bool {
	switch strings.TrimSpace(mode) {
	case PhoneModeCellular, PhoneModeVoLTE, PhoneModeModemVoice:
		return true
	default:
		return false
	}
}

// PhoneServiceEnabled reports the persisted "phone on" bit.
// The card-policy column remains VoWiFiEnabled; native VoLTE and
// cellular software IMS reuse it as the phone-service switch.
func PhoneServiceEnabled(cfg config.DeviceConfig) bool {
	return cfg.VoWiFiEnabled
}

func cellularAlwaysData(cfg config.DeviceConfig) bool {
	return strings.TrimSpace(cfg.PhoneMode) == PhoneModeCellular && strings.TrimSpace(cfg.DataStrategy) == "always"
}

// WorkerSoftwareIMSBlocked reports whether the SIM's MCC is blocked by the
// software IMS policy. Phone service must use modem call control.
func WorkerSoftwareIMSBlocked(w *Worker) bool {
	if w == nil {
		return false
	}
	w.cacheMu.RLock()
	mcc := strings.TrimSpace(w.state.Identity.NativeMCC)
	imsi := strings.TrimSpace(w.state.Identity.IMSI)
	w.cacheMu.RUnlock()
	if carrier.IsVoWiFiBlockedMCC(mcc) {
		return true
	}
	if len(imsi) >= 3 && carrier.IsVoWiFiBlockedMCC(imsi[:3]) {
		return true
	}
	return false
}

// CellularIMSUnavailableReason reports the modem data-path limitation, independent of SIM policy.
func (w *Worker) CellularIMSUnavailableReason() string {
	if w.NetworkController() != nil {
		return ""
	}
	return w.atDeviceRules().cellularIMSUnavailableReason
}

// CheckPhoneModeSupported checks the device and SIM capabilities for a phone mode.
func (w *Worker) CheckPhoneModeSupported(mode string) error {
	if reason := w.atDeviceRules().nativeVoiceUnavailableReason; reason != "" &&
		(UsesModemPhoneControl(mode) || WorkerSoftwareIMSBlocked(w)) {
		return &PhoneModeUnsupportedError{Reason: reason}
	}
	if strings.TrimSpace(mode) == PhoneModeCellular {
		if reason := w.CellularIMSUnavailableReason(); reason != "" {
			return &PhoneModeUnsupportedError{Reason: reason}
		}
	}
	return nil
}

func forceSoftwareIMSBlockedToVoLTE(w *Worker, pol cardpolicy.Policy) {
	if w == nil || !PhoneServiceEnabled(w.Config) || !WorkerSoftwareIMSBlocked(w) || IsModemVoiceMode(w.Config.PhoneMode) {
		return
	}
	w.Config.PhoneMode = PhoneModeVoLTE
	if !pol.AirplaneEnabled {
		w.Config.AirplaneEnabled = false
	}
}

// applyPhoneRadioPolicy applies RF side-effects of turning phone service on.
// Camped modes keep RF on; WiFi calling takes the radio. Callers must retain
// the existing card-specific RF lock checks before applying this projection.
func applyPhoneRadioPolicy(cfg *config.DeviceConfig) {
	if cfg == nil || !PhoneServiceEnabled(*cfg) {
		return
	}
	if PhoneModeCampsOnCell(cfg.PhoneMode) {
		cfg.AirplaneEnabled = false
		if cellularAlwaysData(*cfg) {
			cfg.NetworkEnabled = true
		}
		return
	}
	cfg.AirplaneEnabled = true
	cfg.NetworkEnabled = false
}
