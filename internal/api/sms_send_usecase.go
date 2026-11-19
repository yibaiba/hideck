package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yibaiba/hideck/internal/db"
	"github.com/yibaiba/hideck/internal/device"
	"github.com/yibaiba/hideck/pkg/smscodec"

	"github.com/yibaiba/hideck/pkg/logger"

	"github.com/gin-gonic/gin"
)

func (s *Server) useSendSMS(c businessContext) {
	releaseOperation, allowed := s.acquireBusinessMutation(c)
	if !allowed {
		return
	}
	defer releaseOperation()

	type SendSMSRequest struct {
		DeviceID string `json:"device_id"`
		IMSI     string `json:"imsi"`
		ICCID    string `json:"iccid"`
		Phone    string `json:"phone" binding:"required"`
		Message  string `json:"message" binding:"required"`
		Encoding string `json:"encoding"`
	}

	var req SendSMSRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "参数错误: " + err.Error()})
		return
	}

	encoding, err := smscodec.NormalizeSMSEncoding(req.Encoding)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "短信编码参数错误: " + err.Error()})
		return
	}
	worker, status, msg := s.resolveSMSSendWorker(req.DeviceID, req.IMSI, req.ICCID)
	if status != 0 {
		c.JSON(status, gin.H{"status": "error", "message": msg})
		return
	}
	deviceID := worker.ID
	sendOpts := smscodec.SubmitOptions{Encoding: encoding}

	messageID := ""
	partsTotal := 1
	deliveryState := "acked"

	routed, err := s.pool.SendRoutedSMS(businessRequestContext(c), worker, req.Phone, req.Message, sendOpts)
	if respondOutboundLimit(c, err) {
		return
	}
	if routed.Outcome.PartsTotal > 0 {
		partsTotal = routed.Outcome.PartsTotal
	}
	if strings.TrimSpace(routed.Outcome.DeliveryState) != "" {
		deliveryState = strings.TrimSpace(routed.Outcome.DeliveryState)
	}
	messageID = strings.TrimSpace(routed.Outcome.MessageID)
	if err != nil {
		if routed.Via == device.RoutedSMSViaCS &&
			(deliveryState == db.SMSDeliveryStateUnknown || deliveryState == db.SMSDeliveryStatePartialAck) {
			if persistErr := saveRoutedSMSRecord(routedSMSRecord{
				Identity: routed.Identity, Sender: worker.ID, Recipient: req.Phone,
				Content: req.Message, Status: db.SMSStatusSubmitUnconfirmed,
			}); persistErr != nil {
				logger.Warn("短信提交状态未确认记录入库失败", "device", deviceID, "err", persistErr)
			}
			message := "短信发送结果未知，请勿重发整条"
			if deliveryState == db.SMSDeliveryStatePartialAck {
				message = fmt.Sprintf("短信仅确认提交 %d/%d 段，请勿重发整条", routed.PartsConfirmed, partsTotal)
			} else if routed.PartsConfirmed > 0 {
				message = fmt.Sprintf("短信已确认提交 %d/%d 段，整体结果未知，请勿重发整条", routed.PartsConfirmed, partsTotal)
			}
			c.JSON(http.StatusAccepted, gin.H{
				"status":          "uncertain",
				"message":         message,
				"device":          deviceID,
				"phone":           req.Phone,
				"destination":     routed.Destination,
				"message_id":      messageID,
				"parts_total":     partsTotal,
				"parts_confirmed": routed.PartsConfirmed,
				"delivery_state":  deliveryState,
			})
			return
		}
		if routed.Via == device.RoutedSMSViaVoWiFi || routed.FellBackToCS {
			deliveryState = "failed"
			recordErr := device.RecordVoWiFiSMSSendFailure(s.pool, device.VoWiFiSMSSendFailure{
				DeviceID: deviceID, Target: req.Phone, Content: req.Message,
				Identity: routed.Identity, IdentityRead: routed.IdentityRead, At: time.Now(),
			})
			if recordErr != nil {
				logger.Warn("VoWiFi 短信发送失败记录入库失败", "device", deviceID, "err", recordErr)
			}
			c.JSON(http.StatusInternalServerError, gin.H{
				"status":         "error",
				"message":        "VoWiFi 短信发送失败: " + err.Error(),
				"device":         deviceID,
				"phone":          req.Phone,
				"destination":    routed.Destination,
				"message_id":     messageID,
				"parts_total":    partsTotal,
				"delivery_state": deliveryState,
			})
			return
		}
		if persistErr := saveRoutedSMSRecord(routedSMSRecord{
			Identity: routed.Identity, Sender: worker.ID, Recipient: req.Phone,
			Content: req.Message, Status: 3,
		}); persistErr != nil {
			logger.Warn("短信发送失败记录入库失败", "device", deviceID, "err", persistErr)
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"status":      "error",
			"message":     "发送失败: " + err.Error(),
			"device":      deviceID,
			"phone":       req.Phone,
			"destination": routed.Destination,
		})
		return
	}
	if routed.Via == device.RoutedSMSViaCS {
		if routed.FellBackToCS {
			deliveryState = "acked"
		}
		if persistErr := saveRoutedSMSRecord(routedSMSRecord{
			Identity: routed.Identity, Sender: worker.ID, Recipient: req.Phone,
			Content: req.Message, Status: 2,
		}); persistErr != nil {
			logger.Warn("短信发送成功但入库失败", "device", deviceID, "err", persistErr)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"status":         "ok",
		"message":        "短信已提交",
		"device":         deviceID,
		"phone":          req.Phone,
		"destination":    routed.Destination,
		"message_id":     messageID,
		"parts_total":    partsTotal,
		"delivery_state": deliveryState,
	})
}

func (s *Server) useSMSDelivery(c businessContext) {
	messageID := strings.TrimSpace(c.Param("message_id"))
	if messageID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "message_id 不能为空"})
		return
	}
	if s.pool == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "message": "服务未就绪"})
		return
	}
	services := s.pool.GetAllVoWiFiApps()
	for _, svc := range services {
		if svc == nil {
			continue
		}
		status, err := svc.GetSMSDeliveryStatusContext(businessRequestContext(c), messageID)
		if err != nil {
			continue
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "delivery": status})
		return
	}
	c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "未找到对应短信投递记录"})
}
