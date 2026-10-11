package db

import (
	"errors"
	"testing"
)

func TestLookupSMSIdentityForDevicePinsPhysicalModem(t *testing.T) {
	openTestDB(t)
	for _, row := range []struct{ imei, iccid, imsi string }{
		{"old-modem", "CARD-A", "imsi-a"},
		{"new-modem", "CARD-B", "imsi-b"},
	} {
		if err := DB.Create(&Device{IMEI: row.imei, Alias: "ec20-qmi", CurrentICCID: &row.iccid}).Error; err != nil {
			t.Fatal(err)
		}
		if err := DB.Create(&SIMCard{ICCID: row.iccid, IMSI: row.imsi}).Error; err != nil {
			t.Fatal(err)
		}
	}
	identity, found, err := LookupSMSIdentityForDevice(SMSDeviceSelector{DeviceID: "ec20-qmi", IMEI: "new-modem"})
	if err != nil || !found || identity.ICCID != "CARD-B" || identity.IMSI != "imsi-b" {
		t.Fatalf("identity=%+v found=%v err=%v", identity, found, err)
	}
	_, found, err = LookupSMSIdentityForDevice(SMSDeviceSelector{DeviceID: "ec20-qmi", IMEI: "not-yet-persisted"})
	if err != nil || found {
		t.Fatalf("unknown physical modem must not use alias: found=%v err=%v", found, err)
	}
	old, found, err := LookupDeviceSMSIdentity("old-modem")
	if err != nil || !found || old.ICCID != "CARD-A" {
		t.Fatalf("historical binding changed: %+v found=%v err=%v", old, found, err)
	}
	_, _, err = LookupDeviceSMSIdentity("ec20-qmi")
	if !errors.Is(err, ErrSMSIdentityConflict) {
		t.Fatalf("ambiguous alias error=%v", err)
	}
}

func TestLookupSMSIdentityForDeviceChecksAllAliasBindings(t *testing.T) {
	openTestDB(t)
	for i, iccid := range []string{"CARD-A", "CARD-A", "CARD-B"} {
		row := Device{IMEI: []string{"first", "second", "third"}[i], Alias: "reused", CurrentICCID: &iccid}
		if err := DB.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	_, _, err := LookupDeviceSMSIdentity("reused")
	if !errors.Is(err, ErrSMSIdentityConflict) {
		t.Fatalf("third binding must not be hidden: %v", err)
	}
}
