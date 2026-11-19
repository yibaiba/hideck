package device

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/yibaiba/hideck/internal/db"
)

func TestML307CStoredSMSPersistence(t *testing.T) {
	initDevicePhoneNumberTestDB(t)
	identity := SMSIdentity{ICCID: "8988110000000000001", IMSI: "001010123456789"}
	scope := db.SMSInboundFragmentScope{DeviceID: "c", ICCID: identity.ICCID, IMSI: identity.IMSI}
	fragment := func(seq, minute, second byte, ch rune, observed time.Time, extraUDH ...byte) atInboxPart {
		t.Helper()
		pdu := []byte{
			0,                         // default SMSC
			0x40, 3, 0x81, 0x77, 0xF7, // SMS-DELIVER, UDHI, sender 777
			0, 8, // PID, UCS2
			0x62, 0x11, 0x91, 0x21, minute, second, 0, // SCTS
			byte(8 + len(extraUDH)), byte(5 + len(extraUDH)), 0, 3, 17, 2, seq,
		}
		pdu = append(pdu, extraUDH...)
		pdu = append(pdu, byte(ch>>8), byte(ch))
		part, err := decodeATInboxPart(identity, hex.EncodeToString(pdu))
		if err != nil {
			t.Fatal(err)
		}
		if !part.Concat.IsConcat || part.Sender != "777" || part.Content != string(ch) {
			t.Fatalf("unexpected decoded part: %+v", part)
		}
		part.observedAt = observed
		return part
	}
	load := func(scope db.SMSInboundFragmentScope) *atSMSParts {
		t.Helper()
		state, err := loadATSMSParts(scope)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	apply := func(state *atSMSParts, part atInboxPart, sourceKnown, wantStored bool, wantContent string) {
		t.Helper()
		stored, notification, err := state.persist(part, sourceKnown)
		if err != nil || stored != wantStored {
			t.Fatalf("persist = %t, %v, want %t", stored, err, wantStored)
		}
		if wantContent == "" {
			if notification != nil {
				t.Fatalf("unexpected notification: %+v", notification)
			}
		} else if notification == nil || notification.Content != wantContent {
			t.Fatalf("notification = %+v, want %q", notification, wantContent)
		}
	}

	observed := time.Now()
	a1 := fragment(1, 0, 0, '甲', observed)
	a2 := fragment(2, 0, 0, '乙', observed.Add(2*time.Second))
	b1 := fragment(1, 0, 0x10, '丙', observed.Add(3*time.Second))
	b2 := fragment(2, 0, 0x10, '丁', observed.Add(time.Second))
	state := load(scope)
	apply(state, a1, false, false, "") // unknown ME: no history and no delete.
	apply(state, a1, true, false, "")  // CMTI proves provenance, but an incomplete message retains its source slot.

	state = load(scope)
	if len(state.groups) != 1 {
		t.Fatalf("first fragment was not restored: %+v", state.groups)
	}
	apply(state, b2, true, false, "") // Another message reused the ref inside the collection window.
	if got := state.settledATFragments(observed.Add(time.Second)); len(got) != 0 {
		t.Fatal("merged A1 and B2 before their exact partners arrived")
	}
	apply(state, a2, true, true, "甲乙")
	apply(state, b1, true, true, "丙丁")
	if got := state.settledATFragments(observed.Add(5 * time.Minute)); len(got) != 0 {
		t.Fatal("already complete exact messages must not be merged")
	}

	state = load(scope)
	apply(state, a2, false, true, "") // A failed deletion can retry from durable evidence.
	if err := state.clearCompletedGroups(); err != nil {
		t.Fatal(err)
	}
	if len(load(scope).groups) != 1 {
		t.Fatal("a scan containing a residual slot retired its fragment rows")
	}
	otherDevice := scope
	otherDevice.DeviceID = "c-other"
	if err := load(otherDevice).clearCompletedGroups(); err != nil {
		t.Fatal(err)
	}
	if len(load(scope).groups) != 1 {
		t.Fatal("another module's empty scan retired the original module's fragments")
	}
	if err := load(scope).clearCompletedGroups(); err != nil {
		t.Fatal(err)
	}
	if len(load(scope).groups) != 0 {
		t.Fatal("completed fragment groups were not collected")
	}

	// Legacy groups can still finish after upgrade with their original key.
	legacy1 := fragment(1, 0x05, 0, '旧', observed)
	legacy2 := fragment(2, 0x05, 0, '件', observed)
	legacyScope := scope
	legacyScope.SessionKey = legacy1.key
	if _, err := db.SaveSMSInboundFragment(legacyScope, db.SMSInboundFragment{
		Sequence: 1, Reference: 17, ReferenceBits: 8, Total: 2, Content: legacy1.Content, ArrivedAt: legacy1.Timestamp,
	}); err != nil {
		t.Fatal(err)
	}
	apply(load(scope), legacy2, false, true, "旧件")

	observed = observed.Add(5 * time.Minute)
	c1 := fragment(1, 0x30, 0x95, '跨', observed)
	c2 := fragment(2, 0x40, 0x10, '秒', observed.Add(2*time.Second))
	state = load(scope)
	apply(state, c1, true, false, "")
	apply(state, c2, false, false, "") // Nearby SCTS alone is insufficient proof for an unknown ME slot.
	if len(state.groups) != 2 {
		t.Fatal("unknown ME tail was persisted")
	}
	apply(state, c2, true, false, "")
	state = load(scope) // Process restart before the follow-up full scan.
	apply(state, c1, false, false, "")
	apply(state, c2, false, false, "")
	if messages, err := state.archiveSettledATFragments(observed.Add(time.Minute)); err != nil || len(messages) != 0 {
		t.Fatalf("cross-minute group settled before its collection window elapsed: %+v, %v", messages, err)
	}
	got := state.settledATFragments(observed.Add(3 * time.Minute))
	if len(got) != 1 {
		t.Fatalf("settled cross-minute groups = %d, want 1", len(got))
	}
	key, content, err := completeATMultipart(got[0][0].SessionKey, got[0])
	if err != nil || content != "跨秒" {
		t.Fatalf("complete fragment set: %q, %q, %v", key, content, err)
	}
	messages, err := state.archiveSettledATFragments(observed.Add(3 * time.Minute))
	if err != nil || len(messages) != 1 || messages[0].Content != content {
		t.Fatalf("archive after restart: %+v, %v", messages, err)
	}
	if saved, found, err := db.LookupCompletedReceivedSMS(scope.ICCID, scope.IMSI, key); err != nil || !found || saved.Content != content {
		t.Fatalf("archived history: %+v, %v, %v", saved, found, err)
	}
	state = load(scope)
	apply(state, c1, false, false, "")
	apply(state, c2, false, false, "")
	if messages, err := state.archiveSettledATFragments(observed.Add(4 * time.Minute)); err != nil || len(messages) != 0 {
		t.Fatalf("repeat scan notified an archived SMS twice: %+v, %v", messages, err)
	}

	// A second possible first part within the same family/window makes the
	// entire candidate ambiguous, regardless of how close the older SCTS is.
	otherFirst := fragment(1, 0x40, 0, '混', observed.Add(time.Second))
	apply(state, otherFirst, true, false, "")
	if got := state.settledATFragments(observed.Add(3 * time.Minute)); len(got) != 0 {
		t.Fatal("settled a cross-SCTS group with conflicting sequences")
	}

	port1 := fragment(1, 0x55, 0, '端', observed, 4, 2, 0x10, 0x20)
	port2 := fragment(2, 0x55, 0, '隔', observed, 4, 2, 0x10, 0x21)
	if port1.ApplicationPorts == "" || port1.family == port2.family {
		t.Fatal("8-bit source application ports did not distinguish groups")
	}
	apply(state, port1, true, false, "")
	apply(state, port2, true, false, "")
	port3 := fragment(1, 0x65, 0, '应', observed, 5, 4, 0x0b, 0x01, 0x0b, 0x03)
	port4 := fragment(2, 0x65, 0x10, '离', observed, 5, 4, 0x0b, 0x02, 0x0b, 0x03)
	if port3.ApplicationPorts == "" || port3.family == port4.family {
		t.Fatal("16-bit destination application ports did not distinguish groups")
	}
	apply(state, port3, true, false, "")
	apply(state, port4, true, false, "")
	if messages, err := state.archiveSettledATFragments(observed.Add(3 * time.Minute)); err != nil || len(messages) != 0 {
		t.Fatalf("application-addressed SMS merged across SCTS: %+v, %v", messages, err)
	}

	// A2 is visible in this scan, but its durable write fails. Without that
	// evidence the older A1/B2 pair looks unique and must still stay pending.
	observed = observed.Add(10 * time.Minute)
	x1 := fragment(1, 0x64, 0, '先', observed)
	y2 := fragment(2, 0x64, 0x10, '后', observed.Add(time.Second))
	x2 := fragment(2, 0x64, 0, '正', observed.Add(2*time.Second))
	state = load(scope)
	apply(state, x1, true, false, "")
	apply(state, y2, true, false, "")
	if err := db.DB.Exec(`CREATE TRIGGER test_reject_fragment BEFORE INSERT ON sms_inbound_fragment
		WHEN NEW.content = '正' BEGIN SELECT RAISE(ABORT, 'test fragment write failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if stored, message, err := state.persist(x2, true); err == nil || stored || message != nil {
		t.Fatalf("fault injection failed: %v, %+v, %v", stored, message, err)
	}
	if messages, err := state.archiveSettledATFragments(observed.Add(3 * time.Minute)); err != nil || len(messages) != 0 {
		t.Fatalf("archive ignored a same-family write failure: %+v, %v", messages, err)
	}

	swapped := scope
	swapped.ICCID, swapped.IMSI = "8988110000000000002", "001010123456788"
	apply(load(swapped), a1, false, false, "") // unknown old ME after a card change

	state = load(scope)
	database := db.DB
	db.DB = nil
	stored, _, err := state.persist(a1, true)
	db.DB = database
	if err == nil || stored {
		t.Fatalf("DB failure allowed deleting the slot: %t, %v", stored, err)
	}
}

func TestML307CStoredSMSNoticeSurvivesReadFailure(t *testing.T) {
	w := &Worker{}
	slot := ml307cSMSSlot{store: "ME", index: 7}
	// With identity still unknown, a notification cannot attest a later card.
	w.handleML307CStoredSMS(slot.store, "7")
	if len(w.ml307cSMSNotices.slots) != 0 {
		t.Fatal("unknown SIM claimed an ME slot")
	}
	owner := ml307cSMSOwner{simGeneration: 1}
	notice := &ml307cSMSNotice{owner: owner, slot: slot, observedAt: time.Now()}
	w.ml307cSMSNotices.add(notice) // An unsuccessful CMGR leaves the notification unresolved.

	retry := w.ml307cSMSNotices.snapshot(owner, slot.store)[slot.index]
	if retry != notice || !w.ml307cSMSNotices.bind(retry, "001122") {
		t.Fatal("periodic list could not recover the pending ME notification")
	}
	if w.ml307cSMSNotices.bind(retry, "aabbcc") {
		t.Fatal("retry accepted a replacement after the original PDU was read")
	}

	next := &ml307cSMSNotice{owner: owner, slot: slot, observedAt: time.Now()}
	w.ml307cSMSNotices.add(next)
	w.ml307cSMSNotices.forget(notice)
	if !w.ml307cSMSNotices.bind(next, "aabbcc") {
		t.Fatal("old task discarded a newer slot notification")
	}
	owner.simGeneration++
	if len(w.ml307cSMSNotices.snapshot(owner, slot.store)) != 0 || w.ml307cSMSNotices.bind(next, "aabbcc") {
		t.Fatal("new SIM generation inherited an ME notice")
	}
	latest := &ml307cSMSNotice{owner: owner, slot: slot, observedAt: time.Now()}
	w.ml307cSMSNotices.add(latest)
	if len(w.ml307cSMSNotices.snapshot(next.owner, slot.store)) != 0 || !w.ml307cSMSNotices.current(latest) {
		t.Fatal("an old scan discarded a newer SIM generation's notice")
	}
}
