package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/nyaruka/phonenumbers/v2"
	"github.com/yibaiba/hideck/internal/device"
	"github.com/yibaiba/hideck/internal/phone"
	"github.com/yibaiba/hideck/pkg/logger"
)

const maxPhoneRequestBytes = 1 << 20

type phoneMediaRequest struct {
	SDP string `json:"sdp"`
}

type phoneCallRequest struct {
	DeviceID string `json:"device_id"`
	Callee   string `json:"callee"`
	MediaID  string `json:"media_id"`
}

type phoneControlRequest struct {
	MediaID string `json:"media_id"`
}

type phoneDTMFRequest struct {
	Digit string `json:"digit"`
}

type phoneRefreshRequest struct {
	MediaID  string `json:"media_id"`
	Takeover bool   `json:"takeover"`
}

func (s *Server) registerPhoneRoutes(api *gin.RouterGroup) {
	api.GET("/phone/devices", s.handlePhoneDevices)
	api.POST("/phone/media", s.handlePhoneMedia)
	api.DELETE("/phone/media/:media_id", s.handlePhoneMediaCancel)
	api.POST("/phone/calls", s.handlePhoneStartCall)
	api.GET("/phone/calls/active", s.handlePhoneActiveCalls)
	api.POST("/phone/calls/:call_id/answer", s.handlePhoneAnswer)
	api.POST("/phone/calls/:call_id/reject", s.handlePhoneReject)
	api.POST("/phone/calls/:call_id/dtmf", s.handlePhoneDTMF)
	api.POST("/phone/calls/:call_id/hold", s.handlePhoneHold)
	api.POST("/phone/calls/:call_id/resume", s.handlePhoneResume)
	api.POST("/phone/calls/:call_id/switch", s.handlePhoneSwitch)
	api.DELETE("/phone/calls/:call_id", s.handlePhoneHangup)
	api.PUT("/phone/calls/:call_id/media", s.handlePhoneRefreshMedia)
	api.GET("/phone/events", s.handlePhoneEvents)
	api.GET("/phone/history", s.handlePhoneHistory)
	api.GET("/phone/recordings/:recording", s.handleCommandRecording)
	s.registerPhoneContactRoutes(api)
}

func (s *Server) handlePhoneDevices(c *gin.Context) {
	if !s.requirePhone(c) {
		return
	}
	devices := make([]gin.H, 0)
	if s.pool != nil {
		for _, worker := range s.pool.GetAllWorkers() {
			class, err := device.ClassifyWorkerLebaraUK(worker)
			if err != nil {
				logger.Warn("电话设备列表识别 Lebara UK 射频策略失败", "device", worker.ID, "err", err)
			}
			voice := map[string]interface{}{}
			if s.pool.IsModemVoice(worker.ID) {
				voice = s.pool.ModemVoiceController().DeviceStatus(worker.ID)
			} else if s.pool.IsNativeVoLTE(worker.ID) {
				if ctl := s.pool.NativeVoLTEController(); ctl != nil {
					for key, value := range ctl.DeviceStatus(worker.ID) {
						voice[key] = value
					}
				}
			} else if s.voiceGW != nil {
				voice = s.voiceGW.DeviceStatus(worker.ID)
				for key, value := range s.voiceGW.DeviceStatusCurrent(worker.ID) {
					voice[key] = value
				}
			}
			phoneMode := worker.Config.PhoneMode
			if phoneMode == "" {
				phoneMode = "wifi"
			}
			item := gin.H{
				"id": worker.ID, "name": worker.Config.Name, "iccid": worker.CurrentICCID(),
				"voice":                voice,
				"phone_mode":           phoneMode,
				"data_strategy":        worker.Config.DataStrategy,
				"network_enabled":      worker.Config.NetworkEnabled,
				"vowifi_enabled":       worker.Config.VoWiFiEnabled,
				"vowifi_active":        s.pool.IsVoWiFiActive(worker.ID),
				"native_volte":         s.pool.NativeVoLTEStatus(worker.ID),
				"software_ims_blocked": device.WorkerSoftwareIMSBlocked(worker),
				"rf_lock":              class.RFLock(),
			}
			if region := s.phoneNumberRegion(worker.ID); region != "" {
				item["phone_region"] = region
				if code := phonenumbers.GetCountryCodeForRegion(region); code != 0 {
					item["phone_country_code"] = code
				}
			}
			if recoverSnap := s.pool.LebaraUKIdentityRecoverSnapshot(worker.ID); recoverSnap.Status != "" {
				item["lebara_identity_status"] = recoverSnap.Status
				if recoverSnap.Message != "" {
					item["lebara_identity_message"] = recoverSnap.Message
				}
				if recoverSnap.ICCID != "" {
					item["lebara_identity_iccid"] = recoverSnap.ICCID
				}
			}
			devices = append(devices, item)
		}
	}
	c.JSON(http.StatusOK, gin.H{"devices": devices})
}

func (s *Server) handlePhoneMedia(c *gin.Context) {
	if !s.requirePhone(c) {
		return
	}
	var request phoneMediaRequest
	if !decodePhoneJSON(c, &request) {
		return
	}
	answer, err := s.phone.CreateMedia(c.Request.Context(), s.auth.Username, request.SDP, c.Request.Host)
	if err != nil {
		s.respondPhoneError(c, err)
		return
	}
	c.JSON(http.StatusCreated, answer)
}

func (s *Server) handlePhoneMediaCancel(c *gin.Context) {
	if !s.requirePhone(c) {
		return
	}
	if err := s.phone.CancelMedia(s.auth.Username, c.Param("media_id"), phoneLease(c)); err != nil {
		s.respondPhoneError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) handlePhoneStartCall(c *gin.Context) {
	if !s.requirePhone(c) {
		return
	}
	var request phoneCallRequest
	if !decodePhoneJSON(c, &request) {
		return
	}
	if s.pool != nil {
		if err := s.pool.PrepareCellularCall(c.Request.Context(), request.DeviceID); err != nil {
			s.respondPhoneError(c, err)
			return
		}
	}
	call, err := s.phone.StartCall(phone.StartCallRequest{
		Owner: s.auth.Username, DeviceID: request.DeviceID, Callee: request.Callee,
		MediaID: request.MediaID, Lease: phoneLease(c),
	})
	if err != nil {
		s.respondPhoneError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, call)
}

func (s *Server) handlePhoneActiveCalls(c *gin.Context) {
	if !s.requirePhone(c) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"calls": s.phone.Active(phoneLease(c))})
}

func (s *Server) handlePhoneAnswer(c *gin.Context) {
	if !s.requirePhone(c) {
		return
	}
	request, ok := s.decodePhoneControl(c)
	if !ok {
		return
	}
	call, err := s.phone.Answer(c.Request.Context(), request)
	if err != nil {
		s.respondPhoneError(c, err)
		return
	}
	c.JSON(http.StatusOK, call)
}

func (s *Server) handlePhoneReject(c *gin.Context) {
	if !s.requirePhone(c) {
		return
	}
	request, ok := s.decodePhoneControl(c)
	if !ok {
		return
	}
	if err := s.phone.Reject(request); err != nil {
		s.respondPhoneError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) handlePhoneDTMF(c *gin.Context) {
	if !s.requirePhone(c) {
		return
	}
	var request phoneDTMFRequest
	if !decodePhoneJSON(c, &request) {
		return
	}
	err := s.phone.DTMF(s.auth.Username, c.Param("call_id"), phoneLease(c), request.Digit)
	if err != nil {
		s.respondPhoneError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) handlePhoneHold(c *gin.Context) {
	if !s.requirePhone(c) {
		return
	}
	err := s.phone.Hold(c.Request.Context(), s.auth.Username, c.Param("call_id"), phoneLease(c))
	if err != nil {
		s.respondPhoneError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) handlePhoneResume(c *gin.Context) {
	if !s.requirePhone(c) {
		return
	}
	err := s.phone.Resume(c.Request.Context(), s.auth.Username, c.Param("call_id"), phoneLease(c))
	if err != nil {
		s.respondPhoneError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) handlePhoneSwitch(c *gin.Context) {
	if !s.requirePhone(c) {
		return
	}
	err := s.phone.Switch(s.auth.Username, c.Param("call_id"), phoneLease(c))
	if err != nil {
		s.respondPhoneError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) handlePhoneHangup(c *gin.Context) {
	if !s.requirePhone(c) {
		return
	}
	err := s.phone.Hangup(c.Request.Context(), s.auth.Username, c.Param("call_id"), phoneLease(c))
	if err != nil {
		s.respondPhoneError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) handlePhoneRefreshMedia(c *gin.Context) {
	if !s.requirePhone(c) {
		return
	}
	var request phoneRefreshRequest
	if !decodePhoneJSON(c, &request) {
		return
	}
	call, lease, err := s.phone.RefreshMedia(phone.RefreshRequest{
		Owner: s.auth.Username, CallID: c.Param("call_id"), MediaID: request.MediaID,
		Lease: phoneLease(c), Takeover: request.Takeover,
	})
	if err != nil {
		s.respondPhoneError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"call": call, "lease": lease})
}

func (s *Server) handlePhoneHistory(c *gin.Context) {
	if !s.requirePhone(c) {
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	records, err := s.phone.History(c.Request.Context(), limit)
	if err != nil {
		s.respondPhoneError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"records": records})
}

func (s *Server) decodePhoneControl(c *gin.Context) (phone.ControlRequest, bool) {
	var request phoneControlRequest
	if !decodePhoneJSON(c, &request) {
		return phone.ControlRequest{}, false
	}
	return phone.ControlRequest{
		Owner: s.auth.Username, CallID: c.Param("call_id"), MediaID: request.MediaID, Lease: phoneLease(c),
	}, true
}

func decodePhoneJSON(c *gin.Context, target interface{}) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxPhoneRequestBytes)
	if err := c.ShouldBindJSON(target); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "code": "phone_invalid_request", "message": err.Error()})
		return false
	}
	return true
}

func (s *Server) requirePhone(c *gin.Context) bool {
	if s.phone != nil {
		return true
	}
	c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "code": "phone_unavailable", "message": "电话服务未启用"})
	return false
}

func (s *Server) respondPhoneError(c *gin.Context, err error) {
	if respondOutboundLimit(c, err) {
		return
	}
	message := err.Error()
	status := http.StatusBadRequest
	code := "phone_error"
	switch {
	case errors.Is(err, phone.ErrHoldUnavailable):
		status = http.StatusServiceUnavailable
		code = "phone_hold_unavailable"
		message = phone.ErrHoldUnavailable.Error()
	case strings.Contains(message, "lease"), strings.Contains(message, "another browser"):
		status = http.StatusForbidden
	case strings.Contains(message, "not found"):
		status = http.StatusNotFound
	case strings.Contains(message, "already has"), strings.Contains(message, "busy"):
		status = http.StatusConflict
	case strings.Contains(message, "unavailable"):
		status = http.StatusServiceUnavailable
	}
	c.JSON(status, gin.H{"status": "error", "code": code, "message": message})
}

func phoneLease(c *gin.Context) string {
	return strings.TrimSpace(c.GetHeader("X-Phone-Lease"))
}
