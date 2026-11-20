package db

import (
	"path/filepath"
	"testing"
	"time"
)

func TestATSMSReceiptsBackfillExistingHistory(t *testing.T) {
	previousDB := DB
	path := filepath.Join(t.TempDir(), "receipt.db")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeSMSFragmentTestDB(t)
		DB = previousDB
	})
	legacy := SMS{
		ICCID: "8944100000000000088", IMSI: "234150000000088", Peer: "+10086",
		Sender: "+10086", Content: "legacy", Timestamp: time.Now(), Type: 1,
		FragmentSessionKey: ATSMSInboundPrefix + "legacy",
	}
	// Write directly to emulate a version that persisted no receipt.
	if err := DB.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	closeSMSFragmentTestDB(t)
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := DeleteSMSByID(legacy.ID); err != nil {
		t.Fatal(err)
	}
	input := ReceivedMultipartSMS{
		ICCID: legacy.ICCID, IMSI: legacy.IMSI, Sender: legacy.Sender, Content: legacy.Content,
		Timestamp: legacy.Timestamp, FragmentSessionKey: legacy.FragmentSessionKey,
	}
	for _, imsi := range []string{legacy.IMSI, "234150000000089"} {
		input.IMSI = imsi
		result, err := SaveReceivedMultipartSMS(input)
		if err != nil || !result.Duplicate || result.Created {
			t.Fatalf("backfilled duplicate for %s: %+v, %v", imsi, result, err)
		}
	}
	if found, err := HasATSMSReceipt(input.ICCID, input.IMSI, input.FragmentSessionKey); err != nil || found {
		t.Fatalf("different IMSI acquired deletion evidence: %v, %v", found, err)
	}
}

func TestATSMSReceiptRollsBackWhenHistoryWriteFails(t *testing.T) {
	openTestDB(t)
	input := ReceivedMultipartSMS{
		ICCID: "8944100000000000088", IMSI: "234150000000088", Sender: "+10086",
		Timestamp: time.Now(), Content: "retry", FragmentSessionKey: ATSMSInboundPrefix + "write-failure",
	}
	if err := DB.Exec(`CREATE TRIGGER reject_at_sms BEFORE INSERT ON sms
		BEGIN SELECT RAISE(ABORT, 'test SMS write failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if result, err := SaveReceivedMultipartSMS(input); err == nil || result.Created {
		t.Fatalf("fault injection: %+v, %v", result, err)
	}
	if found, err := HasATSMSReceipt(input.ICCID, input.IMSI, input.FragmentSessionKey); err != nil || found {
		t.Fatalf("failed history write left a receipt: %v, %v", found, err)
	}
	if err := DB.Exec("DROP TRIGGER reject_at_sms").Error; err != nil {
		t.Fatal(err)
	}
	if result, err := SaveReceivedMultipartSMS(input); err != nil || !result.Created {
		t.Fatalf("retry: %+v, %v", result, err)
	}
}
