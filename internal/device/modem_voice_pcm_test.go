package device

import "testing"

func TestVoiceUSBUsesCurrentATOwnership(t *testing.T) {
	modems := []CompatibleModem{
		{USBPath: "/sys/usb/1-1", ATPorts: []string{"/dev/ttyUSB2"}},
		{USBPath: "/sys/usb/2-4", ATPorts: []string{"/dev/ttyUSB6"}},
	}
	path, err := voiceUSBForATPort(modems, "/dev/ttyUSB6")
	if err != nil || path != "/sys/usb/2-4" {
		t.Fatalf("path=%q err=%v", path, err)
	}
	if _, err := voiceUSBForATPort(modems, "/dev/ttyUSB9"); err == nil {
		t.Fatal("accepted missing management port")
	}
	modems = append(modems, CompatibleModem{USBPath: "/sys/usb/3-5", ATPorts: []string{"/dev/ttyUSB6"}})
	if _, err := voiceUSBForATPort(modems, "/dev/ttyUSB6"); err == nil {
		t.Fatal("accepted ambiguous USB ownership")
	}
}
