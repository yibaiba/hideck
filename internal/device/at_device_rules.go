package device

import "github.com/yibaiba/hideck/internal/modem"

type atDeviceRules struct {
	verifyLiveIMEI               bool
	strictSIMIdentity            bool
	simStatusSource              string
	model                        string
	storedSMSInbox               bool
	nativeVoiceUnavailableReason string
}

func atDeviceRulesFor(profile modem.ATProfile) atDeviceRules {
	switch profile {
	case modem.ATProfileML307C:
		return atDeviceRules{
			verifyLiveIMEI:               true,
			strictSIMIdentity:            true,
			simStatusSource:              "ml307c_sim_status",
			model:                        "ML307C",
			storedSMSInbox:               true,
			nativeVoiceUnavailableReason: "ML307C 在 HiDeck 中不支持原生 VoLTE 和模组直拨",
		}
	default:
		return atDeviceRules{model: "EC20"}
	}
}

func (w *Worker) atDeviceRules() atDeviceRules {
	return atDeviceRulesFor(w.atProfile())
}

func (w *Worker) atProfile() modem.ATProfile {
	if w == nil || w.Modem == nil {
		return modem.ATProfileDefault
	}
	return w.Modem.ATProfile()
}

func (w *Worker) isML307C() bool {
	return w.atProfile() == modem.ATProfileML307C
}
