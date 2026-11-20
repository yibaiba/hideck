package db

import (
	"errors"
	"strings"

	"gorm.io/gorm"
)

// atSMSReceipt keeps the receive session after its user-visible history is removed.
type atSMSReceipt struct {
	ICCID      string `gorm:"column:iccid;primaryKey"`
	IMSI       string `gorm:"column:imsi"`
	SessionKey string `gorm:"column:session_key;primaryKey"`
}

func (atSMSReceipt) TableName() string { return "sms_at_receipt" }

func backfillATSMSReceipts(database *gorm.DB) error {
	return database.Exec(`INSERT INTO sms_at_receipt (iccid, imsi, session_key)
		SELECT iccid, imsi, fragment_session_key FROM sms
		WHERE type = 1 AND incomplete = false AND fragment_session_key GLOB ?
		ON CONFLICT (iccid, session_key) DO NOTHING`, ATSMSInboundPrefix+"*").Error
}

func HasATSMSReceipt(iccid, imsi, sessionKey string) (bool, error) {
	if DB == nil {
		return false, errors.New("database not initialized")
	}
	var row atSMSReceipt
	err := DB.Where("iccid = ? AND imsi = ? AND session_key = ?", CanonicalICCID(iccid), strings.TrimSpace(imsi), sessionKey).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return err == nil, err
}
