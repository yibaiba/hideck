package device

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yibaiba/hideck/internal/backend"
	"github.com/yibaiba/hideck/internal/modem"
	qmicore "github.com/yibaiba/hideck/internal/qmi"
	"github.com/yibaiba/hideck/internal/smsnotify"
	"github.com/yibaiba/hideck/pkg/logger"
	"github.com/yibaiba/hideck/pkg/smscodec"

	qmimanager "github.com/iniwex5/quectel-qmi-go/pkg/manager"
	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
)

func (w *Worker) smsQMICore() qmiSMSCore {
	if w == nil {
		return nil
	}
	if w.qmiSMS != nil {
		return w.qmiSMS
	}
	return w.QMICore
}

func (w *Worker) qmiSMSStorageHasIndex(storage uint8, index uint32) bool {
	smsCore := w.smsQMICore()
	if smsCore == nil {
		return false
	}
	for _, tag := range []qmi.MessageTagType{qmi.TagTypeMTNotRead, qmi.TagTypeMTRead} {
		messages, err := smsCore.ListSMS(storage, tag)
		if err != nil {
			continue
		}
		for _, msg := range messages {
			if msg.Index == index {
				return true
			}
		}
	}
	return false
}

func (w *Worker) readIncomingSMSQMI(storage uint8, index uint32) (*qmimanager.DecodedSMS, uint8, error) {
	smsCore := w.smsQMICore()
	if smsCore == nil {
		return nil, storage, fmt.Errorf("qmi sms core not available")
	}

	if storage == 0 || storage == 1 {
		sms, err := smsCore.ReadSMS(storage, index)
		if err == nil {
			return sms, storage, nil
		}

		alternate := uint8(1)
		if storage == 1 {
			alternate = 0
		}
		if w.qmiSMSStorageHasIndex(alternate, index) {
			logger.Warn(fmt.Sprintf("[%s] 主存储读取失败，切换备用存储重试", w.ID),
				"index", index,
				"primary_storage", storage,
				"fallback_storage", alternate,
				"err", err,
			)
			sms, fallbackErr := smsCore.ReadSMS(alternate, index)
			if fallbackErr == nil {
				return sms, alternate, nil
			}
			return nil, alternate, fallbackErr
		}
		return nil, storage, err
	}

	for _, candidate := range []uint8{1, 0} {
		sms, err := smsCore.ReadSMS(candidate, index)
		if err == nil {
			return sms, candidate, nil
		}
	}

	return nil, storage, fmt.Errorf("QMI 短信读取失败: index=%d", index)
}

func (w *Worker) CheckAllSMSQMI() error {
	if w.cellularRadioIsSuppressed() {
		logger.Debug("QMI 短信轮询跳过：蜂窝射频被 VoWiFi/飞行策略抑制", "device", w.ID)
		return nil
	}
	smsCore := w.smsQMICore()
	if smsCore == nil {
		return fmt.Errorf("qmi sms core not available")
	}

	type qmiStoredSMS struct {
		storage uint8
		index   uint32
	}

	seen := make(map[string]struct{})
	discovered := make([]qmiStoredSMS, 0)
	readResiduals := make([]qmiStoredSMS, 0)
	var errs []error
	successes := 0

	for _, storage := range []uint8{0, 1} {
		messages, err := smsCore.ListSMS(storage, qmi.TagTypeMTNotRead)
		if err != nil {
			errs = append(errs, fmt.Errorf("storage %d: %w", storage, err))
			continue
		}
		successes++
		for _, msg := range messages {
			key := fmt.Sprintf("%d:%d", storage, msg.Index)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			discovered = append(discovered, qmiStoredSMS{storage: storage, index: msg.Index})
		}

		messages, err = smsCore.ListSMS(storage, qmi.TagTypeMTRead)
		if err != nil {
			errs = append(errs, fmt.Errorf("storage %d read: %w", storage, err))
			continue
		}
		successes++
		for _, msg := range messages {
			key := fmt.Sprintf("%d:%d", storage, msg.Index)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			readResiduals = append(readResiduals, qmiStoredSMS{storage: storage, index: msg.Index})
		}
	}

	if successes == 0 && len(errs) > 0 {
		return errors.Join(errs...)
	}
	if len(discovered) == 0 && len(readResiduals) == 0 {
		return nil
	}

	if len(discovered) > 0 {
		logger.Info(fmt.Sprintf("[%s] QMI 轮询发现 %d 条未读短信", w.ID, len(discovered)))
	}
	for _, msg := range discovered {
		w.handleNewSMSQMI(msg.storage, msg.index)
	}
	if len(readResiduals) > 0 {
		logger.Info(fmt.Sprintf("[%s] QMI 轮询清理 %d 条已读残留短信", w.ID, len(readResiduals)))
	}
	if len(readResiduals) > 0 {
		if _, err := w.resolveSMSIdentity(); err != nil {
			logger.Warn(fmt.Sprintf("[%s] QMI 短信身份未就绪，保留已读残留短信", w.ID),
				"count", len(readResiduals), "err", err)
			return nil
		}
	}
	for _, msg := range readResiduals {
		if err := smsCore.WMSDeleteMessage(context.Background(), msg.storage, msg.index); err != nil {
			logger.Warn(fmt.Sprintf("[%s] 清理已读残留短信失败 (QMI)", w.ID), "index", msg.index, "storage", msg.storage, "err", err)
		}
	}
	return nil
}

func (w *Worker) handleNewSMSQMI(storage uint8, index uint32) {
	logger.Info(fmt.Sprintf("[%s] 收到新短信通知 (QMI)", w.ID), "index", index, "storage", storage)
	if w.cellularRadioIsSuppressed() {
		logger.Info("QMI 短信通知保留在模组：蜂窝射频被 VoWiFi/飞行策略抑制",
			"device", w.ID, "index", index, "storage", storage)
		return
	}
	identity, err := w.resolveSMSIdentity()
	if err != nil {
		logger.Warn(fmt.Sprintf("[%s] QMI 短信身份未就绪，保留在模组", w.ID),
			"index", index, "storage", storage, "err", err)
		return
	}

	sms, actualStorage, err := w.readIncomingSMSQMI(storage, index)
	if err != nil {
		logger.Error(fmt.Sprintf("[%s] 读取短信失败 (QMI)", w.ID), "index", index, "storage", storage, "err", err)
		return
	}

	if err := w.processDecodedSMSQMIWithIdentity(identity, sms); err != nil {
		logger.Warn(fmt.Sprintf("[%s] QMI 短信未处理，保留在模组", w.ID),
			"index", index, "storage", actualStorage, "err", err)
		return
	}

	if smsCore := w.smsQMICore(); smsCore != nil {
		if err := smsCore.WMSDeleteMessage(context.Background(), actualStorage, index); err != nil {
			logger.Warn(fmt.Sprintf("[%s] 删除短信失败 (QMI)", w.ID), "index", index, "storage", actualStorage, "err", err)
		}
		return
	}
	if w.Backend != nil {
		if err := w.Backend.DeleteSMS(context.Background(), int(index)); err != nil {
			logger.Warn(fmt.Sprintf("[%s] 删除短信失败 (Backend)", w.ID), "index", index, "err", err)
		}
	} else if w.Modem != nil {
		if err := w.Modem.DeleteSMS(index); err != nil {
			logger.Warn(fmt.Sprintf("[%s] 删除短信失败", w.ID), "index", index, "err", err)
		}
	}
}

func (w *Worker) handleNewSMSRawQMI(info qmicore.RawSMSIndication) {
	logger.Info(fmt.Sprintf("[%s] 收到新短信通知 (QMI Raw)", w.ID),
		"pdu_len", len(info.PDU),
		"ack_required", info.AckRequired,
		"transaction_id", info.TransactionID,
		"format", info.Format,
	)
	if w.cellularRadioIsSuppressed() {
		logger.Info("QMI 原始短信通知未处理：蜂窝射频被 VoWiFi/飞行策略抑制",
			"device", w.ID,
			"ack_required", info.AckRequired,
			"transaction_id", info.TransactionID,
		)
		return
	}

	sms, err := qmimanager.DecodeIncomingSMSPDU(info.PDU, qmiSMSStorageUnknown, ^uint32(0))
	if err != nil {
		logger.Error(fmt.Sprintf("[%s] 解析原始短信失败 (QMI)", w.ID),
			"pdu_len", len(info.PDU),
			"pdu_hex", rawSMSPDUHexForLog(info.PDU),
			"ack_required", info.AckRequired,
			"transaction_id", info.TransactionID,
			"format", info.Format,
			"err", err,
		)
		w.ackRawSMSQMI(info, "decode_failure")
		return
	}

	if err := w.processDecodedSMSQMI(sms); err != nil {
		logger.Warn(fmt.Sprintf("[%s] QMI 原始短信未处理，不发送 ACK", w.ID),
			"transaction_id", info.TransactionID, "err", err)
		return
	}
	w.ackRawSMSQMI(info, "processed")
}

func (w *Worker) ackRawSMSQMI(info qmicore.RawSMSIndication, reason string) {
	if !info.AckRequired {
		return
	}
	smsCore := w.smsQMICore()
	if smsCore == nil {
		logger.Warn(fmt.Sprintf("[%s] QMI 原始短信需要 ACK 但 SMS Core 不可用", w.ID),
			"transaction_id", info.TransactionID,
			"reason", reason,
		)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := smsCore.AckRawSMS(ctx, info, true); err != nil {
		logger.Warn(fmt.Sprintf("[%s] QMI 原始短信 ACK 失败", w.ID),
			"transaction_id", info.TransactionID,
			"reason", reason,
			"err", err,
		)
	}
}

func rawSMSPDUHexForLog(pdu []byte) string {
	const maxRawSMSLogBytes = 128
	if len(pdu) <= maxRawSMSLogBytes {
		return strings.ToUpper(hex.EncodeToString(pdu))
	}
	return strings.ToUpper(hex.EncodeToString(pdu[:maxRawSMSLogBytes])) + "...(truncated)"
}

func (w *Worker) processDecodedSMSQMI(sms *qmimanager.DecodedSMS) error {
	identity, err := w.resolveSMSIdentity()
	if err != nil {
		return err
	}
	return w.processDecodedSMSQMIWithIdentity(identity, sms)
}

func (w *Worker) processDecodedSMSQMIWithIdentity(identity SMSIdentity, sms *qmimanager.DecodedSMS) error {
	if sms == nil {
		return errors.New("QMI 短信为空")
	}

	if sms.IsConcat {
		concat := smscodec.ConcatInfo{
			IsConcat: true,
			Ref:      sms.ConcatRef,
			Total:    sms.ConcatTotal,
			Seq:      sms.ConcatSeq,
		}
		logger.Debug(fmt.Sprintf("[%s] 收到 QMI 短信分片", w.ID), "ref", sms.ConcatRef, "seq", sms.ConcatSeq, "total", sms.ConcatTotal)
		complete, full := w.reassembler.Add(sms.Sender, concat, sms.Message)
		if !complete {
			return nil
		}
		sms.Message = full
		logger.Info(fmt.Sprintf("[%s] QMI 长短信重组完成", w.ID), "total", sms.ConcatTotal)
	}

	return w.processSMSWithIdentity(identity, inboundSMSRecord{
		Sender: sms.Sender, Content: sms.Message, Timestamp: sms.Timestamp,
	})
}

func (w *Worker) cleanupFragmentCache(ttl time.Duration) {
	if w == nil || w.reassembler == nil {
		return
	}
	w.reassembler.Cleanup(ttl)
}

func (w *Worker) getIMEI() string {
	return w.getIMEIWithContext(context.Background())
}

func (w *Worker) getIMEIWithContext(ctx context.Context) string {
	if ctx == nil {
		ctx = context.Background()
	}
	if w.Backend != nil {
		if v, err := w.Backend.GetIMEI(ctx); err == nil && v != "" {
			return v
		}
		if w.Backend.Mode() != "at" {
			return ""
		}
	}
	if w.Modem != nil {
		return w.Modem.GetIMEI()
	}
	return ""
}

func (w *Worker) getSMSCWithContext(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if w == nil {
		return "", fmt.Errorf("worker 为空")
	}
	if w.Backend != nil {
		if provider, ok := w.Backend.(backend.SMSCProvider); ok {
			v, err := provider.GetSMSC(ctx)
			return strings.TrimSpace(v), err
		}
		if w.Backend.Mode() != backend.BackendAT {
			return "", fmt.Errorf("backend=%s 未实现 SMSCProvider", w.Backend.Mode())
		}
	}
	if w.Modem != nil {
		v, err := w.Modem.QuerySMSC()
		return strings.TrimSpace(v), err
	}
	return "", nil
}

func (w *Worker) getPhoneNumberWithContext(ctx context.Context) string {
	if ctx == nil {
		ctx = context.Background()
	}
	if w == nil {
		return ""
	}
	if w.Backend != nil {
		if v, err := w.Backend.GetMSISDN(ctx); err == nil {
			if phone := strings.TrimSpace(v); phone != "" {
				return phone
			}
		}
	}
	if w.Modem != nil {
		if v, err := w.Modem.QueryMSISDN(); err == nil {
			if phone := strings.TrimSpace(v); phone != "" {
				return phone
			}
		}
	}
	return ""
}

func (w *Worker) SendSMS(phone, message string) error {
	return w.SendSMSWithOptions(phone, message, smscodec.SubmitOptions{})
}

func (w *Worker) SendSMSWithOptions(phone, message string, opts smscodec.SubmitOptions) error {
	return w.SendSMSForIdentity(context.Background(), phone, message, opts, SMSIdentity{})
}

// SendSMSForIdentity binds a SIM selector to a direct backend send.
func (w *Worker) SendSMSForIdentity(ctx context.Context, phone, message string, opts smscodec.SubmitOptions, selector SMSIdentity) error {
	if w == nil || w.Pool == nil {
		return errors.New("短信发送需要有效的设备池")
	}
	release, err := w.Pool.acquireOutboundOperation(w)
	if err != nil {
		return err
	}
	defer release()
	explicitSelector := selector != (SMSIdentity{})
	selector = normalizeSMSIdentity(selector)
	check := w.Pool.smsSendOwnerCheck(w, selector)
	request := outboundSMSRequest{Worker: w, To: phone, Text: message, Options: opts}
	var intended SMSIdentity
	if w.atDeviceRules().strictSIMIdentity {
		intended, err = w.strictSMSSendIdentity()
		if err != nil {
			return err
		}
	} else if explicitSelector {
		intended = w.cachedSMSIdentity()
	}
	if explicitSelector {
		if !selector.matches(intended) {
			return ErrSMSSelectorMismatch
		}
		if err := check(); err != nil {
			return err
		}
		request.CheckOwner = check
	}
	if err := w.Pool.authorizeSMS(ctx, request); err != nil {
		return err
	}
	if err := w.confirmCSSMSIdentity(ctx, intended, check); err != nil {
		if explicitSelector && errors.Is(err, ErrSMSIdentityConflict) {
			return ErrSMSSelectorMismatch
		}
		return err
	}
	return w.sendSMSWithOptions(phone, message, opts)
}

// sendSMSWithOptions is the CS leg after the shared per-SIM budget was consumed.
func (w *Worker) sendSMSWithOptions(phone, message string, opts smscodec.SubmitOptions) error {
	if w.Backend != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		if sender, ok := w.Backend.(interface {
			SendSMSWithOptions(context.Context, string, string, smscodec.SubmitOptions) error
		}); ok {
			return sender.SendSMSWithOptions(ctx, phone, message, opts)
		}
		if encoding, _ := smscodec.NormalizeSMSEncoding(string(opts.Encoding)); encoding != smscodec.SMSEncodingAuto {
			return fmt.Errorf("设备 %s 的短信后端不支持编码选项: %s", w.ID, opts.Encoding)
		}
		return w.Backend.SendSMS(ctx, phone, message)
	}
	if w.Modem != nil {
		return w.Modem.SendSMSWithOptions(phone, message, opts)
	}
	return fmt.Errorf("设备 %s 无可用的短信发送后端", w.ID)
}

func (w *Worker) GetIMSI() string {
	if w.Backend != nil {
		if v, err := w.Backend.GetIMSI(context.Background()); err == nil && v != "" {
			return v
		}
		if w.Backend.Mode() != "at" {
			return ""
		}
	}
	if w.Modem != nil {
		return w.Modem.GetIMSI()
	}
	return ""
}

func (w *Worker) GetDeviceStatus() modem.DeviceStatus {
	_ = w.RefreshRuntime(nil, "get_device_status")
	return w.ProjectDeviceStatus()
}

func (w *Worker) IsDeviceHealthy() bool {
	healthy, _ := w.ProbeDeviceHealth()
	return healthy
}

func (w *Worker) ProbeDeviceHealth() (bool, error) {
	if w != nil && w.Backend != nil && w.Backend.Mode() == backend.BackendPCSC {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return w.Backend.IsSimInserted(ctx)
	}
	if w.Backend != nil && w.Backend.Mode() != "at" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := w.Backend.GetOperatingMode(ctx)
		if err != nil {
			logger.Debug(fmt.Sprintf("[%s] 控制面健康探测失败", w.ID),
				"event", "control_health_check_failed",
				"backend", w.Backend.Mode(),
				"recovery_remaining_ms", w.healthRecoveryRemaining(time.Now()).Milliseconds(),
				"err", err)
			return false, err
		}
		return true, nil
	}
	if w.Modem != nil {
		return w.Modem.IsHealthy(), nil
	}
	return false, nil
}

func (w *Worker) processSMS(sender, content string, timestamp time.Time) error {
	identity, err := w.resolveSMSIdentity()
	if err != nil {
		return err
	}
	return w.processSMSWithIdentity(identity, inboundSMSRecord{
		Sender: sender, Content: content, Timestamp: timestamp,
	})
}

func (w *Worker) processSMSWithIdentity(identity SMSIdentity, message inboundSMSRecord) error {
	logger.Info(fmt.Sprintf("[%s] 处理新短信", w.ID), "sender", message.Sender, "content_len", len(message.Content))

	if smsnotify.ShouldSuppressReceivedSMS(message.Content) {
		logger.Info(fmt.Sprintf("[%s] 短信已过滤（运营商 OTA/不可解码二进制包）", w.ID), "sender", message.Sender)
		return nil
	}
	message.Timestamp = correctSMSCTimestamp(w.ID, identity.IMSI, message.Timestamp, time.Now())

	if err := w.persistReceivedSMS(identity, message); err != nil {
		return fmt.Errorf("保存短信到数据库失败: %w", err)
	}
	inbound := InboundSMS{
		DeviceID: w.ID, ICCID: identity.ICCID, IMSI: identity.IMSI, Sender: message.Sender,
		Content: message.Content, Time: message.Timestamp,
	}
	w.Pool.notifyInboundSMS(inbound)

	if notifier := w.Pool.getNotifier(); notifier != nil {
		notifySMS(notifier, inbound, "")
	}
	return nil
}
