package device

import (
	"testing"

	"github.com/yibaiba/hideck/internal/modem"
)

func TestDisplayOperatorNameVoWiFiUsesHomeSPNNotServingCOPS(t *testing.T) {
	status := modem.DeviceStatus{
		Operator:  "中国联通",
		NativeSPN: "VOXI",
		NativeMCC: "234",
		NativeMNC: "15",
	}
	got := DisplayOperatorName(status, true)
	if got != "VOXI" {
		t.Fatalf("DisplayOperatorName(vowifi)=%q want VOXI", got)
	}
	if got := DisplayOperatorName(status, false); got != "中国联通" {
		t.Fatalf("DisplayOperatorName(cellular)=%q want 中国联通", got)
	}
}

func TestDisplayOperatorNameVoWiFiFallsBackToHomePLMN(t *testing.T) {
	status := modem.DeviceStatus{
		Operator:  "中国联通",
		NativeMCC: "234",
		NativeMNC: "15",
	}
	got := DisplayOperatorName(status, true)
	if got != "Vodafone UK / VOXI" {
		t.Fatalf("DisplayOperatorName(vowifi plmn)=%q want Vodafone UK / VOXI", got)
	}
}

func TestDisplayOperatorNameVoWiFiUsesIMSIWhenNativePLMNEmpty(t *testing.T) {
	status := modem.DeviceStatus{
		Operator: "中国联通",
		IMSI:     "234150000000001",
	}
	got := DisplayOperatorName(status, true)
	if got != "Vodafone UK / VOXI" {
		t.Fatalf("DisplayOperatorName(vowifi imsi)=%q want Vodafone UK / VOXI", got)
	}
}

func TestDisplayOperatorNameVoWiFiPrefersPNNWhenSPNEmpty(t *testing.T) {
	status := modem.DeviceStatus{
		Operator: "中国联通",
		PNN:      []modem.PNNRecord{{FullName: "giffgaff"}},
	}
	got := DisplayOperatorName(status, true)
	if got != "giffgaff" {
		t.Fatalf("DisplayOperatorName(vowifi pnn)=%q want giffgaff", got)
	}
}
