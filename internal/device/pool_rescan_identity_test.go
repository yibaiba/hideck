package device

import (
	"github.com/yibaiba/hideck/internal/config"
	"testing"
	"time"
)

func TestRescanReusedPathsDoNotInheritOldWorkerIMEI(t *testing.T) {
	original := resolveDiscoveredQMIDeviceFn
	originalAT := probeIMEICachedFn
	t.Cleanup(func() { resolveDiscoveredQMIDeviceFn = original; probeIMEICachedFn = originalAT })
	probeIMEICachedFn = func(string, time.Duration) (string, error) { return "", nil }
	resolveDiscoveredQMIDeviceFn = func(dev QMIDevice, _ time.Duration, _ bool) (QMIDevice, string) {
		if dev.ControlPath == "/dev/cdc-wdm0" {
			return dev, "222222222222222"
		}
		return dev, "111111111111111"
	}
	old := &Worker{ID: "a", Config: config.DeviceConfig{ID: "a", ModemIMEI: "111111111111111", ControlDevice: "/dev/cdc-wdm0", USBPath: "/sys/usb/1-1", Interface: "wwan0"}}
	discovered := []QMIDevice{
		{ControlPath: "/dev/cdc-wdm0", USBPath: "/sys/usb/1-1", NetInterface: "wwan0"},
		{ControlPath: "/dev/cdc-wdm1", USBPath: "/sys/usb/1-2", NetInterface: "wwan1"},
	}
	hardware, _ := (&Pool{}).collectRescanHardware(discovered, BuildWorkerDiscoveryIndex([]*Worker{old}, false), compatibleATDiscoveryNone)
	if len(hardware) != 2 || hardware[0].IMEI != "222222222222222" || hardware[1].IMEI != "111111111111111" {
		t.Fatalf("identities = %+v", hardware)
	}
	result := ResolveDeviceIdentities(hardware, []config.DeviceConfig{old.Config})
	if len(result.Matched) != 1 || result.Matched[0].Hardware.ControlPath != "/dev/cdc-wdm1" {
		t.Fatalf("wrong attachment: %+v", result)
	}
}
