package device

import (
	"strings"

	"github.com/yibaiba/hideck/internal/carrierquery"
	"github.com/yibaiba/hideck/internal/modem"
)

func isUnknownOperatorName(name string) bool {
	name = strings.TrimSpace(name)
	return name == "" || strings.EqualFold(name, "unknown") || name == "未知网络"
}

// DisplayOperatorName is the operator shown next to VoWiFi/cellular in /list and dashboard cards.
// VoWiFi uses SIM home identity, not the serving cell, so a UK VOXI card that still
// reports a Chinese COPS PLMN does not show up as 中国联通 VoWiFi.
func DisplayOperatorName(status modem.DeviceStatus, vowifiActive bool) string {
	if vowifiActive {
		return simHomeOperatorName(status)
	}
	name := strings.TrimSpace(status.Operator)
	if isUnknownOperatorName(name) {
		return ""
	}
	return name
}

func simHomeOperatorName(status modem.DeviceStatus) string {
	if spn := strings.TrimSpace(status.NativeSPN); spn != "" {
		return spn
	}
	for _, rec := range status.PNN {
		if name := strings.TrimSpace(rec.FullName); name != "" {
			return name
		}
		if name := strings.TrimSpace(rec.ShortName); name != "" {
			return name
		}
	}
	mcc, mnc := homeMCCMNC(status)
	if mcc == "" || mnc == "" {
		return ""
	}
	if rule, ok := carrierquery.FindBuiltIn(mcc, mnc); ok {
		if name := strings.TrimSpace(rule.Operator); name != "" {
			return name
		}
	}
	plmn := mcc + mnc
	if name, ok := modem.LookupServingOperatorNameFromPLMN(plmn); ok {
		return name
	}
	return ""
}

func homeMCCMNC(status modem.DeviceStatus) (string, string) {
	mcc := strings.TrimSpace(status.NativeMCC)
	mnc := strings.TrimSpace(status.NativeMNC)
	if mcc != "" && mnc != "" {
		return mcc, mnc
	}
	imsi := strings.TrimSpace(status.IMSI)
	if len(imsi) < 5 {
		return "", ""
	}
	parsedMCC, parsedMNC, _, _, err := modem.HomeMCCMNCFromIMSIAndEFAD(imsi, nil)
	if err != nil {
		return "", ""
	}
	return parsedMCC, parsedMNC
}
