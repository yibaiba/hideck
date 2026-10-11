package db

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

var (
	ErrSMSIdentityUnknown  = errors.New("sms identity unknown")
	ErrSMSIdentityConflict = errors.New("sms identity conflict")
)

type SMSIdentity struct {
	ICCID string
	IMSI  string
}

type SMSRecord struct {
	Identity   SMSIdentity
	LocalPhone string
	Sender     string
	Recipient  string
	Content    string
	Type       int
	Status     int
	Timestamp  time.Time
}

// SMSDeviceSelector pins a lookup to the physical modem when its IMEI is known.
// DeviceID is only used for legacy bindings without a physical identity.
type SMSDeviceSelector struct {
	DeviceID string
	IMEI     string
}

func LookupDeviceSMSIdentity(deviceID string) (SMSIdentity, bool, error) {
	return LookupSMSIdentityForDevice(SMSDeviceSelector{DeviceID: deviceID})
}

func LookupSMSIdentityForDevice(selector SMSDeviceSelector) (SMSIdentity, bool, error) {
	deviceID := strings.TrimSpace(selector.DeviceID)
	imei := strings.TrimSpace(selector.IMEI)
	if DB == nil || (deviceID == "" && imei == "") {
		return SMSIdentity{}, false, nil
	}

	query := DB.Where("alias = ? OR imei = ?", deviceID, deviceID)
	if imei != "" {
		query = DB.Where("imei = ?", imei)
	}
	var devices []Device
	if err := query.Select("iccid").Find(&devices).Error; err != nil {
		return SMSIdentity{}, false, err
	}
	identity, found, err := uniqueBoundSMSIdentity(devices)
	if err != nil || !found {
		return identity, found, err
	}

	var sim SIMCard
	err = DB.Where("iccid = ?", identity.ICCID).First(&sim).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return identity, true, nil
	}
	if err != nil {
		return SMSIdentity{}, false, err
	}
	identity.IMSI = strings.TrimSpace(sim.IMSI)
	return identity, true, nil
}

func uniqueBoundSMSIdentity(devices []Device) (SMSIdentity, bool, error) {
	var resolved SMSIdentity
	for _, device := range devices {
		if device.CurrentICCID == nil {
			continue
		}
		iccid := CanonicalICCID(*device.CurrentICCID)
		if iccid == "" {
			continue
		}
		if resolved.ICCID != "" && resolved.ICCID != iccid {
			return SMSIdentity{}, false, fmt.Errorf("%w: device query matched multiple ICCID bindings", ErrSMSIdentityConflict)
		}
		resolved.ICCID = iccid
	}
	return resolved, resolved.ICCID != "", nil
}

func ResolveICCIDForIMSI(imsi string) (string, error) {
	imsi = strings.TrimSpace(imsi)
	if imsi == "" {
		return "", ErrSMSIdentityUnknown
	}
	if DB == nil {
		return "", ErrSMSIdentityUnknown
	}

	var rows []SIMCard
	if err := DB.Select("iccid").Where("imsi = ?", imsi).Find(&rows).Error; err != nil {
		return "", err
	}
	unique := make(map[string]struct{})
	for _, row := range rows {
		iccid := CanonicalICCID(row.ICCID)
		if iccid == "" || strings.HasPrefix(iccid, "reader-imsi-") {
			continue
		}
		unique[iccid] = struct{}{}
	}
	if len(unique) == 0 {
		return "imsi:" + imsi, nil
	}
	if len(unique) > 1 {
		return "", fmt.Errorf("%w: IMSI maps to %d ICCIDs", ErrSMSIdentityConflict, len(unique))
	}
	for iccid := range unique {
		return iccid, nil
	}
	return "", ErrSMSIdentityUnknown
}

func SaveSMSForIdentity(input SMSRecord) error {
	if DB == nil {
		return errors.New("database is not initialized")
	}
	input.Identity.ICCID = CanonicalICCID(input.Identity.ICCID)
	input.Identity.IMSI = strings.TrimSpace(input.Identity.IMSI)
	if input.Identity.ICCID == "" || input.Identity.IMSI == "" {
		return ErrSMSIdentityUnknown
	}
	sender := strings.TrimSpace(input.Sender)
	recipient := strings.TrimSpace(input.Recipient)
	sms := SMS{
		IMSI: input.Identity.IMSI, ICCID: input.Identity.ICCID,
		Peer:       normalizeSMSPeer(input.Type, sender, recipient),
		LocalPhone: normalizeSMSLocalPhone(input.Identity.IMSI, input.Type, input.LocalPhone, sender, recipient),
		Sender:     sender, Recipient: recipient, Content: input.Content,
		Type: input.Type, Status: input.Status, Timestamp: input.Timestamp.Truncate(time.Second),
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		if err := validateSMSIdentityMapping(tx, input.Identity); err != nil {
			return err
		}
		if err := tx.Create(&sms).Error; err != nil {
			return err
		}
		if sms.Peer == "" {
			return nil
		}
		return upsertSMSContactFromSMS(tx, &sms)
	})
}

func validateSMSIdentityMapping(tx *gorm.DB, identity SMSIdentity) error {
	var sim SIMCard
	err := tx.Select("imsi").Where("iccid = ?", identity.ICCID).First(&sim).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	storedIMSI := strings.TrimSpace(sim.IMSI)
	if storedIMSI != "" && storedIMSI != identity.IMSI {
		return fmt.Errorf("%w: stored ICCID and received IMSI do not match", ErrSMSIdentityConflict)
	}
	return nil
}
