package device

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/modemvoice/audio"
)

// Resolve from the live management port, not a saved interface or USB path.
// The port uses the device's AT queue, so no competing serial reader is opened.
func (port *modemVoicePort) resolveNativePCM(ctx context.Context) (audio.Endpoint, error) {
	response, err := port.ExecuteATContext(ctx, "AT+CGSN", 2*time.Second)
	if err != nil {
		return audio.Endpoint{}, err
	}
	imei, err := parseADBModemIMEI(response)
	if err != nil {
		return audio.Endpoint{}, err
	}
	if !config.IMEIMatches(port.worker.Config.ModemIMEI, imei) {
		return audio.Endpoint{}, errors.New("模组实时 IMEI 不匹配，不能打开声卡")
	}
	modems, err := DiscoverCompatibleModems()
	if err != nil {
		return audio.Endpoint{}, err
	}
	usb, err := voiceUSBForATPort(modems, port.worker.ResolvedATPort())
	if err != nil {
		return audio.Endpoint{}, err
	}
	if modemUACUnusable(usb) {
		return audio.Endpoint{}, errors.New("模组 USB 声卡不可用（打开会破坏 QMI），拒绝原生音频")
	}
	return audio.ResolveUSBPCM(usb)
}

func voiceUSBForATPort(modems []CompatibleModem, atPort string) (string, error) {
	paths := make(map[string]struct{})
	for _, modem := range modems {
		if modemOwnsATPort(modem, atPort) && modem.USBPath != "" {
			paths[modem.USBPath] = struct{}{}
		}
	}
	if len(paths) != 1 {
		return "", fmt.Errorf("管理口对应 %d 个 USB 设备，无法定位模组声卡", len(paths))
	}
	for path := range paths {
		return path, nil
	}
	return "", errors.New("未找到模组 USB 设备")
}
