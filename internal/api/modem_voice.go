package api

import (
	"errors"

	"github.com/yibaiba/hideck/internal/device"
)

var errModemVoiceCallActive = errors.New("请先挂断模组直拨通话，再切换或关闭电话服务")

func (s *Server) checkModemVoiceModeChange(id, nextMode string) error {
	if s.pool == nil || s.pool.ModemVoiceController() == nil {
		return nil
	}
	controller := s.pool.ModemVoiceController()
	if controller.ActiveCall(id) == nil || nextMode == device.PhoneModeModemVoice {
		return nil
	}
	return errModemVoiceCallActive
}
