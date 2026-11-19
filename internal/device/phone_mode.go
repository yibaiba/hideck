package device

import (
	"errors"
	"strings"

	"github.com/iniwex5/vowifi-go/runtimehost/carrier"
	"github.com/yibaiba/hideck/internal/cardpolicy"
	"github.com/yibaiba/hideck/internal/config"
)

var ErrModemVoiceSoftwareIMS = errors.New("模组直拨模式不使用软件 IMS")

var ErrML307CNativeVoiceUnavailable = errors.New("ML307C 在 HiDeck 中不支持原生 VoLTE 和模组直拨")

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
	mcc := strings.TrimSpace(w.state.Identity.NativeMCC)
	if carrier.IsVoWiFiBlockedMCC(mcc) {
		return true
	}
	imsi := strings.TrimSpace(w.state.Identity.IMSI)
	if len(imsi) >= 3 && carrier.IsVoWiFiBlockedMCC(imsi[:3]) {
		return true
	}
	return false
}

// CheckPhoneModeSupported applies ML307C's native voice limit, including SIMs
// whose carrier policy requires native call control.
func (w *Worker) CheckPhoneModeSupported(mode string) error {
	if w.isML307C() && (UsesModemPhoneControl(mode) || WorkerSoftwareIMSBlocked(w)) {
		return ErrML307CNativeVoiceUnavailable
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
