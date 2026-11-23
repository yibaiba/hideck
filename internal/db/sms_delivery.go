package db

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	SMSDeliveryStatePending    = "pending"
	SMSDeliveryStatePartialAck = "partial_ack"
	SMSDeliveryStateAcked      = "acked"
	SMSDeliveryStateFailed     = "failed"
	SMSDeliveryStateUnknown    = "unknown"
)

const (
	SMSDeliveryPartStatePending = "pending"
	SMSDeliveryPartStateAcked   = "acked"
	SMSDeliveryPartStateFailed  = "failed"
	SMSDeliveryPartStateTimeout = "timeout"
)

// SMSDelivery 记录一条上行短信(message 级别)的发送追踪状态。
type SMSDelivery struct {
	MessageID  string    `gorm:"column:message_id;primaryKey" json:"message_id"`
	IMSI       string    `gorm:"column:imsi;index" json:"imsi"`
	ICCID      string    `gorm:"column:iccid;index" json:"iccid"`
	DeviceID   string    `gorm:"column:device_id;index" json:"device_id"`
	Peer       string    `gorm:"column:peer;index" json:"peer"`
	Content    string    `gorm:"column:content" json:"content"`
	PartsTotal int       `gorm:"column:parts_total" json:"parts_total"`
	Acks       int       `gorm:"column:acks" json:"acks"`
	State      string    `gorm:"column:state;index" json:"state"`
	LastError  string    `gorm:"column:last_error" json:"last_error"`
	CreatedAt  time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt  time.Time `gorm:"column:updated_at" json:"updated_at"`
}

func (SMSDelivery) TableName() string { return "sms_delivery" }

// SMSDeliveryPart 记录一条上行短信分片(part 级别)的发送与回执状态。
type SMSDeliveryPart struct {
	ID        uint       `gorm:"primaryKey" json:"id"`
	MessageID string     `gorm:"column:message_id;uniqueIndex:idx_sms_delivery_part_mid_no,priority:1;index" json:"message_id"`
	PartNo    int        `gorm:"column:part_no;uniqueIndex:idx_sms_delivery_part_mid_no,priority:2" json:"part_no"`
	CallID    string     `gorm:"column:call_id;index" json:"call_id"`
	InReplyTo string     `gorm:"column:in_reply_to;index" json:"in_reply_to"`
	RPMR      int        `gorm:"column:rp_mr;index" json:"rp_mr"`
	State     string     `gorm:"column:state;index" json:"state"`
	SIPCode   int        `gorm:"column:sip_code" json:"sip_code"`
	RPCause   int        `gorm:"column:rp_cause" json:"rp_cause"`
	ErrorText string     `gorm:"column:error_text" json:"error_text"`
	SentAt    time.Time  `gorm:"column:sent_at;index" json:"sent_at"`
	ReportAt  *time.Time `gorm:"column:report_at;index" json:"report_at,omitempty"`
	CreatedAt time.Time  `gorm:"column:created_at" json:"created_at"`
	UpdatedAt time.Time  `gorm:"column:updated_at" json:"updated_at"`
}

func (SMSDeliveryPart) TableName() string { return "sms_delivery_part" }

// SMSDeliveryStatus 用于 API 返回 message 及其分片状态。
type SMSDeliveryStatus struct {
	MessageID  string            `json:"message_id"`
	IMSI       string            `json:"imsi"`
	ICCID      string            `json:"iccid"`
	DeviceID   string            `json:"device_id"`
	Peer       string            `json:"peer"`
	Content    string            `json:"content"`
	PartsTotal int               `json:"parts_total"`
	Acks       int               `json:"acks"`
	State      string            `json:"state"`
	LastError  string            `json:"last_error"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
	Parts      []SMSDeliveryPart `json:"parts"`
}

func UpsertSMSDeliveryPart(messageID string, partNo int, callID string, rpMR int, state string, sentAt time.Time) error {
	if DB == nil {
		return nil
	}
	messageID = strings.TrimSpace(messageID)
	if messageID == "" || partNo <= 0 {
		return errors.New("message_id/part_no 非法")
	}
	if sentAt.IsZero() {
		sentAt = time.Now()
	}
	if strings.TrimSpace(state) == "" {
		state = SMSDeliveryPartStatePending
	}
	part := SMSDeliveryPart{
		MessageID: messageID,
		PartNo:    partNo,
		CallID:    strings.TrimSpace(callID),
		RPMR:      rpMR,
		State:     strings.TrimSpace(state),
		SentAt:    sentAt,
		CreatedAt: sentAt,
		UpdatedAt: sentAt,
	}
	return DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "message_id"}, {Name: "part_no"}},
		DoUpdates: clause.Assignments(map[string]any{
			"call_id":    part.CallID,
			"rp_mr":      part.RPMR,
			"state":      part.State,
			"sent_at":    part.SentAt,
			"updated_at": sentAt,
		}),
	}).Create(&part).Error
}

func MarkSMSDeliveryPartSIPResult(
	messageID string,
	partNo, sipCode int,
	state, errText string,
	at time.Time,
) error {
	if DB == nil {
		return nil
	}
	messageID = strings.TrimSpace(messageID)
	if messageID == "" || partNo <= 0 {
		return errors.New("message_id/part_no 非法")
	}
	if at.IsZero() {
		at = time.Now()
	}
	result := DB.Model(&SMSDeliveryPart{}).
		Where("message_id = ? AND part_no = ?", messageID, partNo).
		Updates(map[string]any{
			"state": gorm.Expr(
				"CASE WHEN report_at IS NULL THEN ? ELSE state END",
				strings.TrimSpace(state),
			),
			"sip_code": sipCode,
			"error_text": gorm.Expr(
				"CASE WHEN report_at IS NULL THEN ? ELSE error_text END",
				strings.TrimSpace(errText),
			),
			"updated_at": at,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func MarkSMSDeliveryPartReport(inReplyTo, callID, deviceID string, rpMR int, state string, sipCode int, rpCause int, errText string, at time.Time) (SMSDeliveryPart, error) {
	if DB == nil {
		return SMSDeliveryPart{}, gorm.ErrRecordNotFound
	}
	if at.IsZero() {
		at = time.Now()
	}
	state = strings.TrimSpace(state)
	if state == "" {
		state = SMSDeliveryPartStateFailed
	}

	deviceID = strings.TrimSpace(deviceID)
	baseQuery := func() *gorm.DB {
		q := DB.Model(&SMSDeliveryPart{})
		if deviceID != "" {
			q = q.Joins("JOIN sms_delivery ON sms_delivery.message_id = sms_delivery_part.message_id").
				Where("sms_delivery.device_id = ?", deviceID)
		}
		return q
	}

	findLatest := func(q *gorm.DB) (SMSDeliveryPart, error) {
		var p SMSDeliveryPart
		err := q.Order("sms_delivery_part.created_at desc").First(&p).Error
		return p, err
	}

	inReplyTo = strings.TrimSpace(inReplyTo)
	callID = strings.TrimSpace(callID)

	var part SMSDeliveryPart
	var err error
	if keys := smsCallIDLookupValues(inReplyTo); len(keys) > 0 {
		part, err = findLatest(baseQuery().Where("sms_delivery_part.call_id IN ?", keys))
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return SMSDeliveryPart{}, err
		}
		if part.ID == 0 {
			return SMSDeliveryPart{}, gorm.ErrRecordNotFound
		}
	} else {
		if callID != "" {
			part, err = findLatest(baseQuery().Where("sms_delivery_part.call_id IN ?", smsCallIDLookupValues(callID)))
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return SMSDeliveryPart{}, err
			}
		}
		if part.ID == 0 && rpMR >= 0 {
			cutoff := at.Add(-120 * time.Second)
			part, err = findLatest(baseQuery().
				Where("sms_delivery_part.rp_mr = ? AND sms_delivery_part.created_at >= ?", rpMR, cutoff).
				Where("sms_delivery_part.state IN ?", []string{SMSDeliveryPartStatePending, SMSDeliveryPartStateAcked, SMSDeliveryPartStateFailed, SMSDeliveryPartStateTimeout}))
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return SMSDeliveryPart{}, err
			}
		}
		if part.ID == 0 {
			return SMSDeliveryPart{}, gorm.ErrRecordNotFound
		}
	}

	reportAt := at
	updates := map[string]any{
		"in_reply_to": inReplyTo,
		"state":       state,
		"rp_cause":    rpCause,
		"error_text":  strings.TrimSpace(errText),
		"report_at":   &reportAt,
		"updated_at":  at,
	}
	if part.SIPCode == 0 && sipCode > 0 {
		updates["sip_code"] = sipCode
	}
	if err := DB.Model(&SMSDeliveryPart{}).Where("id = ?", part.ID).Updates(updates).Error; err != nil {
		return SMSDeliveryPart{}, err
	}
	if err := DB.First(&part, part.ID).Error; err != nil {
		return SMSDeliveryPart{}, err
	}
	if err := RecomputeSMSDelivery(part.MessageID, at); err != nil {
		return SMSDeliveryPart{}, err
	}
	return part, nil
}

func smsCallIDLookupValues(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	seen := map[string]struct{}{value: {}}
	out := []string{value}
	add := func(candidate string) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			return
		}
		if _, ok := seen[candidate]; ok {
			return
		}
		seen[candidate] = struct{}{}
		out = append(out, candidate)
	}
	trimmed := strings.Trim(value, "<>\"")
	add(trimmed)
	add(strings.ToLower(trimmed))
	return out
}

func RecomputeSMSDelivery(messageID string, at time.Time) error {
	if DB == nil {
		return nil
	}
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return nil
	}
	if at.IsZero() {
		at = time.Now()
	}

	var total int64
	if err := DB.Model(&SMSDeliveryPart{}).Where("message_id = ?", messageID).Count(&total).Error; err != nil {
		return err
	}
	if total == 0 {
		return nil
	}
	var acked int64
	if err := DB.Model(&SMSDeliveryPart{}).Where("message_id = ? AND state = ?", messageID, SMSDeliveryPartStateAcked).Count(&acked).Error; err != nil {
		return err
	}
	var failedPart SMSDeliveryPart
	failErr := DB.Model(&SMSDeliveryPart{}).
		Where("message_id = ? AND state IN ?", messageID, []string{SMSDeliveryPartStateFailed, SMSDeliveryPartStateTimeout}).
		Order("updated_at desc").
		First(&failedPart).Error
	state := SMSDeliveryStatePending
	lastError := ""
	if failErr == nil {
		state = SMSDeliveryStateFailed
		lastError = strings.TrimSpace(failedPart.ErrorText)
	} else if errors.Is(failErr, gorm.ErrRecordNotFound) {
		if acked == total {
			state = SMSDeliveryStateAcked
		} else if acked > 0 {
			state = SMSDeliveryStatePartialAck
		}
	} else {
		return failErr
	}

	return DB.Model(&SMSDelivery{}).Where("message_id = ?", messageID).Updates(map[string]any{
		"acks":       int(acked),
		"state":      state,
		"last_error": lastError,
		"updated_at": at,
	}).Error
}

func GetSMSDeliveryStatus(messageID string) (*SMSDeliveryStatus, error) {
	if DB == nil {
		return nil, gorm.ErrRecordNotFound
	}
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return nil, gorm.ErrRecordNotFound
	}
	var delivery SMSDelivery
	if err := DB.Where("message_id = ?", messageID).First(&delivery).Error; err != nil {
		return nil, err
	}
	var parts []SMSDeliveryPart
	if err := DB.Where("message_id = ?", messageID).Order("part_no asc").Find(&parts).Error; err != nil {
		return nil, err
	}
	out := &SMSDeliveryStatus{
		MessageID:  delivery.MessageID,
		IMSI:       delivery.IMSI,
		ICCID:      delivery.ICCID,
		DeviceID:   delivery.DeviceID,
		Peer:       delivery.Peer,
		Content:    delivery.Content,
		PartsTotal: delivery.PartsTotal,
		Acks:       delivery.Acks,
		State:      delivery.State,
		LastError:  delivery.LastError,
		CreatedAt:  delivery.CreatedAt,
		UpdatedAt:  delivery.UpdatedAt,
		Parts:      parts,
	}
	return out, nil
}

func UpdateSMSDeliveryState(messageID, state, lastError string, acks int, at time.Time) error {
	if DB == nil {
		return nil
	}
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return nil
	}
	if at.IsZero() {
		at = time.Now()
	}
	updates := map[string]any{"updated_at": at}
	if strings.TrimSpace(state) != "" {
		updates["state"] = strings.TrimSpace(state)
	}
	if acks >= 0 {
		updates["acks"] = acks
	}
	updates["last_error"] = strings.TrimSpace(lastError)
	return DB.Model(&SMSDelivery{}).Where("message_id = ?", messageID).Updates(updates).Error
}
