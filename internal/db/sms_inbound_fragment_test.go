package db

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestSMSInboundFragmentPersistsAcrossDatabaseReopen(t *testing.T) {
	previousDB := DB
	path := filepath.Join(t.TempDir(), "fragments.db")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if DB != nil {
			if sqlDB, err := DB.DB(); err == nil {
				_ = sqlDB.Close()
			}
		}
		DB = previousDB
	})
	scope := SMSInboundFragmentScope{
		DeviceID: "wwan0", ICCID: "8944100000000000001",
		IMSI: "234102356143376", SessionKey: "sender=giffgaff|ref=198",
	}
	fragment := SMSInboundFragment{
		Reference: 198, ReferenceBits: 8, Total: 2, Sequence: 1,
		Content: "first ", ArrivedAt: time.Now(), RPMR: 61, CallID: "part-1",
	}
	result, err := SaveSMSInboundFragment(scope, fragment)
	if err != nil || !result.Inserted || len(result.Fragments) != 1 {
		t.Fatalf("save result=%#v err=%v", result, err)
	}
	closeSMSFragmentTestDB(t)
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	rows, err := LoadSMSInboundFragments(scope)
	if err != nil || len(rows) != 1 || rows[0].Content != "first " ||
		!rows[0].ArrivedAt.Equal(fragment.ArrivedAt) {
		t.Fatalf("reopened rows=%#v err=%v", rows, err)
	}
	assertSMSInboundFragmentDuplicateAndCollision(t, scope, fragment)
	ackedAt := time.Now().Add(time.Second)
	if err := MarkSMSInboundFragmentAcked(scope, 1, ackedAt); err != nil {
		t.Fatal(err)
	}
	rows, err = LoadSMSInboundFragments(scope)
	if err != nil || len(rows) != 1 || !rows[0].AckSent || rows[0].AckSentAt.Before(ackedAt) {
		t.Fatalf("acked rows=%#v err=%v", rows, err)
	}
	degradedAt := ackedAt.Add(time.Second)
	if err := MarkSMSInboundFragmentsDegraded(scope, degradedAt); err != nil {
		t.Fatal(err)
	}
	closeSMSFragmentTestDB(t)
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	rows, err = LoadSMSInboundFragments(scope)
	if err != nil || len(rows) != 1 || !rows[0].DegradedAt.Equal(degradedAt) {
		t.Fatalf("degraded rows after reopen=%#v err=%v", rows, err)
	}
	if err := DeleteSMSInboundFragments(scope); err != nil {
		t.Fatal(err)
	}
	rows, err = LoadSMSInboundFragments(scope)
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows after delete=%#v err=%v", rows, err)
	}
}

func TestSMSInboundFragmentsFollowSIMAcrossDeviceMove(t *testing.T) {
	openTestDB(t)
	first := SMSInboundFragmentScope{
		DeviceID: "wwan0", ICCID: "8944100000000000088",
		IMSI: "234150000000088", SessionKey: "sender=service|ref=88",
	}
	fragment := SMSInboundFragment{
		Reference: 88, ReferenceBits: 8, Total: 2, Sequence: 1,
		Content: "first", ArrivedAt: time.Now(),
	}
	if _, err := SaveSMSInboundFragment(first, fragment); err != nil {
		t.Fatal(err)
	}
	moved := first
	moved.DeviceID = "wwan1"
	rows, err := LoadSMSInboundFragments(moved)
	if err != nil || len(rows) != 1 || rows[0].Content != "first" {
		t.Fatalf("moved SIM fragments=%+v err=%v", rows, err)
	}
}

func TestSMSInboundFragmentScopeKeepsSIMsSeparate(t *testing.T) {
	openTestDB(t)
	owner := SMSInboundFragmentScope{
		DeviceID: "wwan0", ICCID: "8944100000000000088", IMSI: "234150000000088",
		SessionKey: "sender=testsender|ref=107|bits=8|local=+12025550100",
	}
	fragment := SMSInboundFragment{Reference: 107, ReferenceBits: 8, Total: 2, Sequence: 1,
		Content: "synthetic fragment", ArrivedAt: time.Now()}
	if _, err := SaveSMSInboundFragment(owner, fragment); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"iccid", "imsi"} {
		t.Run(field, func(t *testing.T) {
			other := owner
			if field == "iccid" {
				other.ICCID = "8944100000000000099"
			} else {
				other.IMSI = "234150000000099"
			}
			rows, err := LoadSMSInboundFragments(other)
			if err != nil || len(rows) != 0 {
				t.Fatalf("other SIM read %d fragments: %v", len(rows), err)
			}
			if err := DeleteSMSInboundFragments(other); err != nil {
				t.Fatal(err)
			}
			rows, err = LoadSMSInboundFragments(owner)
			if err != nil || len(rows) != 1 {
				t.Fatalf("other SIM changed owner fragments: count=%d err=%v", len(rows), err)
			}
		})
	}
}

func assertSMSInboundFragmentDuplicateAndCollision(
	t *testing.T,
	scope SMSInboundFragmentScope,
	fragment SMSInboundFragment,
) {
	t.Helper()
	duplicate, err := SaveSMSInboundFragment(scope, fragment)
	if err != nil || duplicate.Inserted || len(duplicate.Fragments) != 1 {
		t.Fatalf("duplicate result=%#v err=%v", duplicate, err)
	}
	fragment.Content = "changed"
	collision, err := SaveSMSInboundFragment(scope, fragment)
	if !errors.Is(err, ErrSMSInboundFragmentCollision) || collision.CollisionReason != "sequence_content_mismatch" {
		t.Fatalf("collision result=%#v err=%v", collision, err)
	}
}

func closeSMSFragmentTestDB(t *testing.T) {
	t.Helper()
	sqlDB, err := DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	DB = nil
}
