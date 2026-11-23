package db

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
)

const ATSMSInboundPrefix = "at:"

var ErrSMSInboundFragmentCollision = errors.New("SMS inbound fragment collision")

type SMSInboundFragmentScope struct {
	DeviceID   string
	ICCID      string
	IMSI       string
	SessionKey string
}

type SMSInboundFragment struct {
	ID             uint      `gorm:"primaryKey"`
	DeviceID       string    `gorm:"column:device_id;index"`
	ICCID          string    `gorm:"column:iccid"`
	IMSI           string    `gorm:"column:imsi;index"`
	SessionKey     string    `gorm:"column:session_key"`
	Sequence       int       `gorm:"column:sequence"`
	Reference      int       `gorm:"column:reference"`
	ReferenceBits  int       `gorm:"column:reference_bits"`
	Total          int       `gorm:"column:total"`
	Content        string    `gorm:"column:content"`
	PDUFingerprint string    `gorm:"column:pdu_fingerprint"`
	ArrivedAt      time.Time `gorm:"column:arrived_at;index"`
	RPMR           int       `gorm:"column:rp_mr"`
	CallID         string    `gorm:"column:call_id"`
	ToURI          string    `gorm:"column:to_uri"`
	ServiceCenter  string    `gorm:"column:service_center"`
	AckSent        bool      `gorm:"column:ack_sent"`
	AckSentAt      time.Time `gorm:"column:ack_sent_at"`
	DegradedAt     time.Time `gorm:"column:degraded_at;index"`
	CreatedAt      time.Time `gorm:"column:created_at"`
	UpdatedAt      time.Time `gorm:"column:updated_at"`
}

func (SMSInboundFragment) TableName() string { return "sms_inbound_fragment" }

type SMSInboundFragmentSaveResult struct {
	Inserted        bool
	CollisionReason string
	Fragments       []SMSInboundFragment
}

func LoadSMSInboundFragments(owner SMSInboundFragmentScope) ([]SMSInboundFragment, error) {
	if DB == nil {
		return nil, errors.New("database not initialized")
	}
	owner = normalizeSMSInboundFragmentScope(owner)
	var fragments []SMSInboundFragment
	err := DB.Where("iccid = ? AND imsi = ?", owner.ICCID, owner.IMSI).
		Order("session_key ASC, sequence ASC").Find(&fragments).Error
	return fragments, err
}

func SaveSMSInboundFragment(
	scope SMSInboundFragmentScope,
	fragment SMSInboundFragment,
) (SMSInboundFragmentSaveResult, error) {
	if DB == nil {
		return SMSInboundFragmentSaveResult{}, errors.New("database not initialized")
	}
	scope = normalizeSMSInboundFragmentScope(scope)
	if err := validateSMSInboundFragment(scope, fragment); err != nil {
		return SMSInboundFragmentSaveResult{}, err
	}
	var result SMSInboundFragmentSaveResult
	err := DB.Transaction(func(tx *gorm.DB) error {
		inserted, collision, err := saveSMSInboundFragmentRow(tx, scope, fragment)
		result.Inserted, result.CollisionReason = inserted, collision
		if err != nil {
			loadErr := loadSMSInboundFragmentSession(tx, scope, &result.Fragments)
			return errors.Join(err, loadErr)
		}
		return loadSMSInboundFragmentSession(tx, scope, &result.Fragments)
	})
	return result, err
}

func saveSMSInboundFragmentRow(
	tx *gorm.DB,
	scope SMSInboundFragmentScope,
	fragment SMSInboundFragment,
) (bool, string, error) {
	var existing SMSInboundFragment
	err := fragmentScopeQuery(tx, scope).Where("sequence = ?", fragment.Sequence).First(&existing).Error
	if err == nil {
		reason := smsInboundFragmentCollision(existing, fragment)
		if reason != "" {
			return false, reason, ErrSMSInboundFragmentCollision
		}
		return false, "", nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return false, "", err
	}
	now := time.Now()
	fragment.ID = 0
	fragment.DeviceID, fragment.ICCID = scope.DeviceID, scope.ICCID
	fragment.IMSI, fragment.SessionKey = scope.IMSI, scope.SessionKey
	fragment.CreatedAt, fragment.UpdatedAt = now, now
	if err := tx.Create(&fragment).Error; err != nil {
		return false, "", err
	}
	return true, "", nil
}

func DeleteSMSInboundFragments(scope SMSInboundFragmentScope) error {
	if DB == nil {
		return errors.New("database not initialized")
	}
	scope = normalizeSMSInboundFragmentScope(scope)
	if scope.ICCID == "" || scope.IMSI == "" || scope.SessionKey == "" {
		return errors.New("invalid SMS inbound fragment scope")
	}
	return fragmentScopeQuery(DB, scope).Delete(&SMSInboundFragment{}).Error
}

func MarkSMSInboundFragmentAcked(
	scope SMSInboundFragmentScope,
	sequence int,
	at time.Time,
) error {
	if DB == nil {
		return errors.New("database not initialized")
	}
	if at.IsZero() {
		at = time.Now()
	}
	scope = normalizeSMSInboundFragmentScope(scope)
	return fragmentScopeQuery(DB.Model(&SMSInboundFragment{}), scope).
		Where("sequence = ?", sequence).Updates(map[string]any{
		"ack_sent": true, "ack_sent_at": at, "updated_at": at,
	}).Error
}

func MarkSMSInboundFragmentsDegraded(scope SMSInboundFragmentScope, at time.Time) error {
	if DB == nil {
		return errors.New("database not initialized")
	}
	if at.IsZero() {
		return errors.New("missing SMS inbound fragment degraded time")
	}
	scope = normalizeSMSInboundFragmentScope(scope)
	result := fragmentScopeQuery(DB.Model(&SMSInboundFragment{}), scope).
		Updates(map[string]any{"degraded_at": at, "updated_at": at})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("SMS inbound fragment session not found")
	}
	return nil
}

func loadSMSInboundFragmentSession(
	tx *gorm.DB,
	scope SMSInboundFragmentScope,
	destination *[]SMSInboundFragment,
) error {
	return fragmentScopeQuery(tx, scope).Order("sequence ASC").Find(destination).Error
}

func fragmentScopeQuery(tx *gorm.DB, scope SMSInboundFragmentScope) *gorm.DB {
	return tx.Where(
		"iccid = ? AND imsi = ? AND session_key = ?",
		scope.ICCID, scope.IMSI, scope.SessionKey,
	)
}

func normalizeSMSInboundFragmentScope(scope SMSInboundFragmentScope) SMSInboundFragmentScope {
	scope.DeviceID = strings.TrimSpace(scope.DeviceID)
	scope.ICCID = CanonicalICCID(scope.ICCID)
	scope.IMSI = strings.TrimSpace(scope.IMSI)
	scope.SessionKey = strings.TrimSpace(scope.SessionKey)
	return scope
}

func validateSMSInboundFragment(scope SMSInboundFragmentScope, fragment SMSInboundFragment) error {
	if scope.ICCID == "" || scope.IMSI == "" || scope.SessionKey == "" {
		return errors.New("invalid SMS inbound fragment scope")
	}
	if fragment.Total < 2 || fragment.Sequence < 1 || fragment.Sequence > fragment.Total {
		return errors.New("invalid SMS inbound fragment bounds")
	}
	if fragment.ArrivedAt.IsZero() {
		return errors.New("missing SMS inbound fragment arrival time")
	}
	return nil
}

func smsInboundFragmentCollision(existing, candidate SMSInboundFragment) string {
	if existing.Total != candidate.Total {
		return "total_mismatch"
	}
	if strings.TrimSpace(existing.Content) != strings.TrimSpace(candidate.Content) {
		return "sequence_content_mismatch"
	}
	return ""
}
