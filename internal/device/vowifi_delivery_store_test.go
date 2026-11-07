package device

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost/messaging"
	"github.com/yibaiba/hideck/internal/db"
)

var _ messaging.InboundFragmentStore = vowifiDeliveryStore{}
var _ messaging.InboundFragmentLifecycleStore = vowifiDeliveryStore{}
var _ messaging.SubscriptionRejectionStore = vowifiDeliveryStore{}
var _ messaging.MWISubscriptionPolicyStore = vowifiDeliveryStore{}

func TestVoWiFiDeliveryStoreReportsMatchedPart(t *testing.T) {
	previousDB := db.DB
	if err := db.Init(filepath.Join(t.TempDir(), "delivery.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = previousDB
	})

	store := vowifiDeliveryStore{}
	now := time.Now()
	if err := store.CreateSMSDeliveryForIdentity(messaging.DeliveryRecord{
		MessageID: "message-1", ICCID: "iccid-1", IMSI: "imsi-1",
		DeviceID: "wwan0", Peer: "+10086", Content: "hello", PartsTotal: 1, At: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSMSDeliveryPart("message-1", 1, "call-1", 17, "pending", now); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSMSDeliveryPartSIPResult("message-1", 1, 202, "pending", "", now); err != nil {
		t.Fatal(err)
	}
	pending, err := store.GetSMSDeliveryStatus("message-1")
	if err != nil {
		t.Fatal(err)
	}
	if pending.State != "pending" || pending.Parts[0].SIPCode != 202 || pending.Parts[0].ReportAt != nil {
		t.Fatalf("pending SIP result = %+v", pending)
	}
	match, err := store.MarkSMSDeliveryPartReport("call-1", "report-1", "wwan0", 17, "acked", 200, 0, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if !match.Matched || match.MessageID != "message-1" || match.PartNo != 1 || match.State != "acked" {
		t.Fatalf("delivery match = %+v", match)
	}
	completed, err := store.GetSMSDeliveryStatus("message-1")
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != "acked" || completed.Parts[0].SIPCode != 202 || completed.Parts[0].ReportAt == nil {
		t.Fatalf("completed delivery result = %+v", completed)
	}
	if err := store.MarkSMSDeliveryPartSIPResult("message-1", 1, 202, "pending", "late SIP result", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	preserved, err := store.GetSMSDeliveryStatus("message-1")
	if err != nil {
		t.Fatal(err)
	}
	if preserved.Parts[0].State != "acked" || preserved.Parts[0].ReportAt == nil || preserved.Parts[0].ErrorText != "" {
		t.Fatalf("late SIP result downgraded report = %+v", preserved.Parts[0])
	}
	assertInboundFragmentStoreRoundTrip(t, store, now)
}

func TestVoWiFiDeliveryStoreRejectsMismatchedInReplyTo(t *testing.T) {
	previousDB := db.DB
	if err := db.Init(filepath.Join(t.TempDir(), "delivery.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = previousDB
	})

	store := vowifiDeliveryStore{}
	now := time.Now()
	if err := store.CreateSMSDeliveryForIdentity(messaging.DeliveryRecord{
		MessageID: "message-1", ICCID: "iccid-1", IMSI: "imsi-1",
		DeviceID: "wwan0", Peer: "+10086", Content: "hello", PartsTotal: 1, At: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSMSDeliveryPart("message-1", 1, "call-1", 17, "pending", now); err != nil {
		t.Fatal(err)
	}
	_, err := store.MarkSMSDeliveryPartReport("other-call", "report-1", "wwan0", 17, "acked", 200, 0, "", now)
	if !errors.Is(err, messaging.ErrDeliveryNotFound) {
		t.Fatalf("mismatched In-Reply-To err = %v", err)
	}
}

func assertInboundFragmentStoreRoundTrip(t *testing.T, store vowifiDeliveryStore, at time.Time) {
	t.Helper()
	scope := messaging.InboundFragmentScope{
		Owner:      messaging.InboundFragmentOwner{DeviceID: "wwan0", ICCID: "8944100000000000001", IMSI: "imsi-1"},
		SessionKey: "sender=giffgaff|ref=198",
	}
	result, err := store.SaveInboundFragment(scope, messaging.InboundFragment{
		Reference: 198, ReferenceBits: 8, Total: 2, Sequence: 1,
		Content: "first", ArrivedAt: at, RPMR: 61,
	})
	if err != nil || !result.Inserted || len(result.Fragments) != 1 {
		t.Fatalf("fragment save=%#v err=%v", result, err)
	}
	rows, err := store.LoadInboundFragments(scope.Owner)
	if err != nil || len(rows) != 1 || rows[0].Scope != scope || rows[0].Fragment.Content != "first" {
		t.Fatalf("fragment load=%#v err=%v", rows, err)
	}
	degradedAt := at.Add(time.Second)
	if err := store.MarkInboundFragmentsDegraded(scope, degradedAt); err != nil {
		t.Fatal(err)
	}
	rows, err = store.LoadInboundFragments(scope.Owner)
	if err != nil || len(rows) != 1 || !rows[0].Fragment.DegradedAt.Equal(degradedAt) {
		t.Fatalf("fragment degraded state=%#v err=%v", rows, err)
	}
	if err := store.DeleteInboundFragments(scope); err != nil {
		t.Fatal(err)
	}
}

func TestVoWiFiDeliveryStoreMWIPolicy(t *testing.T) {
	previousDB := db.DB
	if err := db.Init(filepath.Join(t.TempDir(), "mwi.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = previousDB
	})
	store := vowifiDeliveryStore{}
	const iccid, identity = "8949020000000000001", "dev\x00impi\x00ims.example\x00sip:user@ims.example"

	if mode, rejected, err := store.LoadMWISubscriptionPolicy(iccid, identity); err != nil || mode != db.MWIModeAuto || rejected != 0 {
		t.Fatalf("card without policy: mode=%q rejected=%d err=%v", mode, rejected, err)
	}
	policy := db.DefaultCardPolicy(iccid)
	policy.MWIMode = db.MWIModeOff
	if err := db.UpsertCardPolicy(policy); err != nil {
		t.Fatal(err)
	}
	if mode, _, err := store.LoadMWISubscriptionPolicy(iccid, identity); err != nil || mode != db.MWIModeOff {
		t.Fatalf("saved setting: mode=%q err=%v", mode, err)
	}
	policy.MWIMode = db.MWIModeOn
	if err := db.UpsertCardPolicy(policy); err != nil {
		t.Fatal(err)
	}
	if mode, _, err := store.LoadMWISubscriptionPolicy(iccid, identity); err != nil || mode != db.MWIModeOn {
		t.Fatalf("updated setting: mode=%q err=%v", mode, err)
	}
	if err := store.SaveMWISubscriptionRejection(iccid, identity, 480); err == nil {
		t.Fatal("480 was saved as an explicit rejection")
	}
	if err := store.SaveMWISubscriptionRejection(iccid, identity, 405); err != nil {
		t.Fatal(err)
	}
	if _, rejected, err := store.LoadMWISubscriptionPolicy(iccid, identity); err != nil || rejected != 405 {
		t.Fatalf("remembered rejection = %d err=%v", rejected, err)
	}
	if _, rejected, _ := store.LoadMWISubscriptionPolicy(iccid, identity+"-other"); rejected != 0 {
		t.Fatal("rejection leaked to another identity")
	}
	if _, rejected, _ := store.LoadMWISubscriptionPolicy("8949020000000000002", identity); rejected != 0 {
		t.Fatal("rejection leaked to another card")
	}
	if err := db.DeleteIMSSubscriptionRejections(identity); err != nil {
		t.Fatal(err)
	}
	if _, rejected, _ := store.LoadMWISubscriptionPolicy(iccid, identity); rejected != 405 {
		t.Fatal("deregistration cleanup removed the MWI rejection")
	}
	expires := time.Now().Add(time.Hour)
	if err := db.SaveIMSSubscriptionRejection(identity, "message-summary", 489, expires); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveIMSSubscriptionRejection(identity, "reg", 489, expires); err != nil {
		t.Fatal(err)
	}
	if cleared, err := db.DeleteIMSMWIRejections(iccid); err != nil || !cleared {
		t.Fatalf("cleared=%v err=%v", cleared, err)
	}
	if _, rejected, _ := store.LoadMWISubscriptionPolicy(iccid, identity); rejected != 0 {
		t.Fatal("clearing the card kept the MWI rejection")
	}
	if status, _, _ := db.LoadIMSSubscriptionRejection(identity, "message-summary", time.Now()); status != 0 {
		t.Fatal("clearing the card kept the registration-scoped MWI rejection")
	}
	if status, _, _ := db.LoadIMSSubscriptionRejection(identity, "reg", time.Now()); status != 489 {
		t.Fatal("clearing MWI removed the reg-event rejection")
	}
	if cleared, err := db.DeleteIMSMWIRejections(iccid); err != nil || cleared {
		t.Fatalf("second clear: cleared=%v err=%v", cleared, err)
	}
}
