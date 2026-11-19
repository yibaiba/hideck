package db

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	smsFragmentSessionIndexName      = "uidx_sms_fragment_session"
	smsFragmentSessionUniqueIndexSQL = "CREATE UNIQUE INDEX " + smsFragmentSessionIndexName +
		" ON sms(iccid, fragment_session_key) WHERE fragment_session_key <> ''"
)

type ReceivedMultipartSMS struct {
	ICCID              string
	IMSI               string
	LocalPhone         string
	Sender             string
	Recipient          string
	Content            string
	FragmentSessionKey string
	Timestamp          time.Time
	Incomplete         bool
}

type ReceivedMultipartSMSResult struct {
	SMSID      uint
	Created    bool
	Reconciled bool
	Duplicate  bool
}

func ensureSMSFragmentSessionUniqueIndex(database *gorm.DB) error {
	if database == nil {
		return errors.New("database not initialized")
	}
	var existing string
	err := database.Raw(
		"SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?", smsFragmentSessionIndexName,
	).Scan(&existing).Error
	if err != nil {
		return err
	}
	compact := compactSQLiteDefinition(existing)
	if compact == compactSQLiteDefinition(smsFragmentSessionUniqueIndexSQL) {
		return nil
	}
	return database.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("DROP INDEX IF EXISTS " + smsFragmentSessionIndexName).Error; err != nil {
			return err
		}
		return tx.Exec(smsFragmentSessionUniqueIndexSQL).Error
	})
}

func compactSQLiteDefinition(definition string) string {
	return strings.ToLower(strings.Join(strings.Fields(definition), ""))
}

func SaveReceivedMultipartSMS(input ReceivedMultipartSMS) (ReceivedMultipartSMSResult, error) {
	if DB == nil {
		return ReceivedMultipartSMSResult{}, errors.New("database not initialized")
	}
	normalized, err := normalizeReceivedMultipartSMS(input)
	if err != nil {
		return ReceivedMultipartSMSResult{}, err
	}
	var result ReceivedMultipartSMSResult
	recordATReceipt := !normalized.Incomplete && strings.HasPrefix(normalized.FragmentSessionKey, ATSMSInboundPrefix)
	receipt := atSMSReceipt{ICCID: normalized.ICCID, IMSI: normalized.IMSI, SessionKey: normalized.FragmentSessionKey}
	smsPublicationMu.Lock()
	defer smsPublicationMu.Unlock()
	err = DB.Transaction(func(tx *gorm.DB) error {
		var existing SMS
		findErr := tx.Where(
			"iccid = ? AND fragment_session_key = ?",
			normalized.ICCID, normalized.FragmentSessionKey,
		).First(&existing).Error
		if errors.Is(findErr, gorm.ErrRecordNotFound) {
			if recordATReceipt {
				insert := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&receipt)
				if insert.Error != nil {
					return insert.Error
				}
				if insert.RowsAffected == 0 {
					result.Duplicate = true
					return nil
				}
			}
			created, createErr := createReceivedMultipartSMS(tx, normalized)
			result = created
			return createErr
		}
		if findErr != nil {
			return findErr
		}
		reconciled, reconcileErr := reconcileReceivedMultipartSMS(tx, existing, normalized)
		result = reconciled
		if reconcileErr != nil {
			return reconcileErr
		}
		if recordATReceipt && existing.IMSI == normalized.IMSI {
			return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&receipt).Error
		}
		return nil
	})
	if err == nil && (result.Created || result.Reconciled) {
		publishIncomingSMSByID(result.SMSID)
	}
	return result, err
}

func normalizeReceivedMultipartSMS(input ReceivedMultipartSMS) (ReceivedMultipartSMS, error) {
	result := input
	result.ICCID = CanonicalICCID(input.ICCID)
	result.IMSI = strings.TrimSpace(input.IMSI)
	result.LocalPhone = strings.TrimSpace(input.LocalPhone)
	result.Sender = strings.TrimSpace(input.Sender)
	result.Recipient = strings.TrimSpace(input.Recipient)
	result.Content = strings.TrimSpace(input.Content)
	result.FragmentSessionKey = strings.TrimSpace(input.FragmentSessionKey)
	if result.IMSI == "" || result.Sender == "" || result.Content == "" || result.FragmentSessionKey == "" {
		return ReceivedMultipartSMS{}, errors.New("invalid received multipart SMS")
	}
	if result.Timestamp.IsZero() {
		return ReceivedMultipartSMS{}, errors.New("missing received multipart SMS timestamp")
	}
	if result.ICCID == "" {
		result.ICCID = GetICCIDForIMSI(result.IMSI)
	}
	result.LocalPhone = normalizeSMSLocalPhoneForIdentity(smsLocalPhoneInput{
		Identity: SMSIdentity{ICCID: result.ICCID, IMSI: result.IMSI}, Type: 1,
		LocalPhone: result.LocalPhone, Sender: result.Sender, Recipient: result.Recipient,
	})
	return result, nil
}

func createReceivedMultipartSMS(
	tx *gorm.DB,
	input ReceivedMultipartSMS,
) (ReceivedMultipartSMSResult, error) {
	sms := SMS{
		IMSI: input.IMSI, ICCID: input.ICCID,
		Peer:       normalizeSMSPeer(1, input.Sender, input.Recipient),
		LocalPhone: input.LocalPhone,
		Sender:     input.Sender, Recipient: input.Recipient, Content: input.Content,
		Type: 1, Status: 0, Timestamp: input.Timestamp.Truncate(time.Second),
		FragmentSessionKey: input.FragmentSessionKey, Incomplete: input.Incomplete,
	}
	if err := tx.Create(&sms).Error; err != nil {
		return ReceivedMultipartSMSResult{}, err
	}
	if sms.Peer != "" {
		if err := upsertSMSContactFromSMS(tx, &sms); err != nil {
			return ReceivedMultipartSMSResult{}, err
		}
	}
	return ReceivedMultipartSMSResult{SMSID: sms.ID, Created: true}, nil
}

func reconcileReceivedMultipartSMS(
	tx *gorm.DB,
	existing SMS,
	input ReceivedMultipartSMS,
) (ReceivedMultipartSMSResult, error) {
	if !existing.Incomplete || input.Incomplete {
		if strings.TrimSpace(existing.Content) != input.Content && existing.Incomplete == input.Incomplete {
			return ReceivedMultipartSMSResult{}, errors.New("received multipart SMS session content conflict")
		}
		return ReceivedMultipartSMSResult{SMSID: existing.ID, Duplicate: true}, nil
	}
	if err := tx.Model(&existing).Updates(map[string]any{
		"content": input.Content, "incomplete": false,
	}).Error; err != nil {
		return ReceivedMultipartSMSResult{}, err
	}
	if _, err := rebuildSMSContactTx(tx, existing.ICCID, existing.Peer); err != nil {
		return ReceivedMultipartSMSResult{}, err
	}
	return ReceivedMultipartSMSResult{SMSID: existing.ID, Reconciled: true}, nil
}
