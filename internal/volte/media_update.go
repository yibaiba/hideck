package volte

import (
	"fmt"
	"net"
	"strings"

	"github.com/yibaiba/hideck/pkg/logger"
)

func (c *Controller) UpdateCallMedia(deviceID, callID, sdp string) error {
	deviceID = strings.TrimSpace(deviceID)
	if c == nil || deviceID == "" {
		return fmt.Errorf("volte: controller or device is unavailable")
	}
	return c.withDevice(deviceID, func() error {
		call, ok := c.lookup(deviceID, callID)
		if !ok || call.State == "completed" {
			return fmt.Errorf("volte: active call %s not found on device %s", callID, deviceID)
		}
		media := c.media.get(callID)
		if media == nil || media.bridge == nil {
			return fmt.Errorf("volte: call %s media bridge is unavailable", callID)
		}
		port, err := rtpPortFromSDP(sdp)
		if err != nil {
			return err
		}
		previous, _ := media.bridge.mediaEndpoint()
		remote := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
		if err := media.bridge.SetRemote(remote); err != nil {
			return fmt.Errorf("volte: update call %s media: %w", callID, err)
		}
		oldPort := 0
		if address, ok := previous.(*net.UDPAddr); ok {
			oldPort = address.Port
		}
		logger.Info("VoLTE 媒体端点已重绑定", "device", deviceID, "call_id", callID,
			"old_local_port", oldPort, "new_local_port", port)
		return nil
	})
}
