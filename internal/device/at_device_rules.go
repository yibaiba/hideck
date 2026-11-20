package device

import "github.com/yibaiba/hideck/internal/modem"

type atDeviceRules struct {
	verifyLiveIMEI               bool
	strictSIMIdentity            bool
	simStatusSource              string
	model                        string
	storedSMSInbox               bool
	nativeVoiceUnavailableReason string
	cellularIMSUnavailableReason string
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
			cellularIMSUnavailableReason: "ML307C 在 HiDeck 中不支持通过模组蜂窝数据使用软件 IMS",
		}
	default:
		return atDeviceRules{model: "EC20"}
	}
}

func (w *Worker) atDeviceRules() atDeviceRules {
	if w == nil || w.Modem == nil {
		return atDeviceRulesFor(modem.ATProfileDefault)
	}
	return atDeviceRulesFor(w.Modem.ATProfile())
}
