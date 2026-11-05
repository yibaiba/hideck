package db

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// IMSMWIRejection remembers an IMS identity whose network explicitly rejected
// SUBSCRIBE(mwi) with 405 or 489 (hideck#97). Unlike IMSSubscriptionRejection
// it has no expiry and survives deregistration; only turning the card's MWI
// setting on clears it.
type IMSMWIRejection struct {
	ICCID        string `gorm:"column:iccid;primaryKey;size:32"`
	IdentityHash string `gorm:"column:identity_hash;primaryKey;size:64"`
	StatusCode   int    `gorm:"column:status_code;not null"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (IMSMWIRejection) TableName() string { return "ims_mwi_rejections" }

// LoadIMSMWIRejection returns the remembered status, or 0 when none.
func LoadIMSMWIRejection(iccid, identity string) (int, error) {
	iccid, identityHash, err := normalizeIMSMWIRejectionKey(iccid, identity)
	if err != nil {
		return 0, err
	}
	if DB == nil {
		return 0, gorm.ErrInvalidDB
	}
	var row IMSMWIRejection
	err = DB.Where("iccid = ? AND identity_hash = ?", iccid, identityHash).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	return row.StatusCode, err
}

func SaveIMSMWIRejection(iccid, identity string, status int) error {
	iccid, identityHash, err := normalizeIMSMWIRejectionKey(iccid, identity)
	if err != nil {
		return err
	}
	if status != 405 && status != 489 {
		return fmt.Errorf("MWI status %d is not an explicit rejection", status)
	}
	if DB == nil {
		return gorm.ErrInvalidDB
	}
	return DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "iccid"}, {Name: "identity_hash"}},
		DoUpdates: clause.AssignmentColumns([]string{"status_code", "updated_at"}),
	}).Create(&IMSMWIRejection{ICCID: iccid, IdentityHash: identityHash, StatusCode: status}).Error
}

// DeleteIMSMWIRejections clears every remembered rejection for the card,
// including the registration-scoped message-summary rejection kept for the
// same identities, and reports whether anything was remembered.
func DeleteIMSMWIRejections(iccid string) (bool, error) {
	iccid = CanonicalICCID(iccid)
	if iccid == "" {
		return false, errors.New("ICCID 为空")
	}
	if DB == nil {
		return false, gorm.ErrInvalidDB
	}
	var hashes []string
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&IMSMWIRejection{}).Where("iccid = ?", iccid).Pluck("identity_hash", &hashes).Error; err != nil {
			return err
		}
		if len(hashes) == 0 {
			return nil
		}
		if err := tx.Where("identity_hash IN ? AND event_package = ?", hashes, "message-summary").
			Delete(&IMSSubscriptionRejection{}).Error; err != nil {
			return err
		}
		return tx.Where("iccid = ?", iccid).Delete(&IMSMWIRejection{}).Error
	})
	return len(hashes) > 0, err
}

func normalizeIMSMWIRejectionKey(iccid, identity string) (string, string, error) {
	iccid = CanonicalICCID(iccid)
	if iccid == "" {
		return "", "", errors.New("ICCID 为空")
	}
	identityHash, _, err := normalizeIMSSubscriptionRejectionKey(identity, "message-summary")
	return iccid, identityHash, err
}
