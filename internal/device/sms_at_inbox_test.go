package device

import (
	"context"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/yibaiba/hideck/internal/db"
)

func TestATStoredSMSPersistence(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "at-sms.db")
	open := func() {
		t.Helper()
		if err := db.Init(dbPath); err != nil {
			t.Fatal(err)
		}
	}
	closeDB := func() {
		t.Helper()
		sqlDB, err := db.DB.DB()
		if err != nil {
			t.Fatal(err)
		}
		if err := sqlDB.Close(); err != nil {
			t.Fatal(err)
		}
	}
	open()
	t.Cleanup(func() {
		closeDB()
		db.DB = nil
	})
	identity := SMSIdentity{ICCID: "8988110000000000001", IMSI: "001010123456789"}
	scope := db.SMSInboundFragmentScope{DeviceID: "c", ICCID: identity.ICCID, IMSI: identity.IMSI}
	multipart := func(total, seq, minute, second byte, ch rune, observed time.Time, extraUDH ...byte) atInboxPart {
		t.Helper()
		pdu := []byte{
			0,                         // default SMSC
			0x40, 3, 0x81, 0x77, 0xF7, // SMS-DELIVER, UDHI, sender 777
			0, 8, // PID, UCS2
			0x62, 0x11, 0x91, 0x21, minute, second, 0, // SCTS
			byte(8 + len(extraUDH)), byte(5 + len(extraUDH)), 0, 3, 17, total, seq,
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
	fragment := func(seq, minute, second byte, ch rune, observed time.Time, extraUDH ...byte) atInboxPart {
		t.Helper()
		return multipart(2, seq, minute, second, ch, observed, extraUDH...)
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

	lookupHistory := func(key string) (db.SMS, bool, error) {
		var row db.SMS
		query := db.DB.Where("iccid = ? AND imsi = ? AND fragment_session_key = ?", scope.ICCID, scope.IMSI, key).
			Limit(1).Find(&row)
		return row, query.RowsAffected == 1, query.Error
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
	// A stale ME slot from another SIM can have the same sender, reference,
	// SCTS and total as this SIM's pending first part.
	otherSIM := scope
	otherSIM.ICCID = "8988110000000000003"
	staleTail := fragment(2, 0, 0, '异', observed)
	apply(load(otherSIM), staleTail, true, false, "")
	apply(state, staleTail, false, false, "")
	if len(state.groups) != 1 {
		t.Fatal("unknown ME tail created another group")
	}
	for _, group := range state.groups {
		if len(group) != 1 {
			t.Fatal("unknown ME tail filled a pending sequence")
		}
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
	// A different, unrecognised UDH IE leaves the decoded text and all
	// grouping metadata equal but changes the raw PDU.
	differentPDU := fragment(2, 0, 0, '乙', observed, 0x70, 1, 0x01)
	if differentPDU.family != a2.family || differentPDU.key != a2.key || differentPDU.fingerprint == a2.fingerprint {
		t.Fatalf("raw mismatch fixture: %+v", differentPDU)
	}
	apply(state, differentPDU, false, false, "")
	exactKey := ""
	for key, group := range state.groups {
		if group[0].Content == a1.Content {
			var err error
			exactKey, _, err = completeATMultipart(key, group)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	exact, found, err := lookupHistory(exactKey)
	if err != nil || !found {
		t.Fatalf("exact archived SMS: %+v, %v, %v", exact, found, err)
	}
	if _, _, _, err := db.DeleteSMSByID(exact.ID); err != nil {
		t.Fatal(err)
	}
	closeDB()
	open()
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

	// A new message can reuse the exact grouping fields and first-part text
	// of a completed one. Its different PDU must have its own receipt.
	first := fragment(1, 0x03, 0, '同', observed)
	firstTail := fragment(2, 0x03, 0, '甲', observed.Add(time.Second))
	second := fragment(1, 0x03, 0, '同', observed.Add(2*time.Second), 0x70, 1, 0x01)
	secondTail := fragment(2, 0x03, 0, '乙', observed.Add(3*time.Second))
	if first.key != second.key || first.family != second.family || first.fingerprint == second.fingerprint {
		t.Fatal("fixture must share grouping fields and have distinct first-part PDUs")
	}
	state = load(scope)
	apply(state, first, true, false, "")
	apply(state, firstTail, true, true, "同甲")
	state = load(scope)
	apply(state, second, true, false, "")
	apply(state, secondTail, true, true, "同乙")
	state = load(scope)
	apply(state, second, false, true, "")
	if err := load(scope).clearCompletedGroups(); err != nil {
		t.Fatal(err)
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
	apply(load(scope), legacy2, false, false, "")
	apply(load(scope), legacy1, false, false, "")
	apply(load(scope), legacy2, true, true, "旧件")
	apply(load(scope), legacy1, false, false, "") // No raw PDU hash on the pre-upgrade row.
	apply(load(scope), legacy1, true, true, "")   // Known sources can still match legacy rows by text.

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
	if messages, err := state.archiveSettledATFragments(observed.Add(3 * time.Minute)); err != nil || len(messages) != 0 {
		t.Fatalf("archived a cross-SCTS SMS using a DB-only group: %+v, %v", messages, err)
	}
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
	saved, found, err := lookupHistory(key)
	if err != nil || !found || saved.Content != content {
		t.Fatalf("archived history: %+v, %v, %v", saved, found, err)
	}
	if err := db.DeleteArchivedRecords(context.Background(), db.DB, db.ArchiveDeleteRequest{SMSIDs: []uint{saved.ID}}); err != nil {
		t.Fatal(err)
	}
	closeDB()
	open()
	state = load(scope)
	apply(state, c1, false, false, "")
	apply(state, c2, false, false, "")
	if messages, err := state.archiveSettledATFragments(observed.Add(4 * time.Minute)); err != nil || len(messages) != 0 {
		t.Fatalf("repeat scan notified an archived SMS twice: %+v, %v", messages, err)
	}
	for _, deletedKey := range []string{exactKey, key} {
		if _, found, err := lookupHistory(deletedKey); err != nil || found {
			t.Fatalf("repeat scan recreated a removed history row: %s, %v, %v", deletedKey, found, err)
		}
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

	// One exact group contains two of the three parts. Observing its first
	// part alone cannot prove the other part is still in modem storage.
	observed = observed.Add(10 * time.Minute)
	p1 := multipart(3, 1, 0x15, 0, '全', observed)
	p2 := multipart(3, 2, 0x15, 0, '部', observed.Add(time.Second))
	p3 := multipart(3, 3, 0x15, 0x10, '在', observed.Add(2*time.Second))
	state = load(scope)
	for _, part := range []atInboxPart{p1, p2, p3} {
		apply(state, part, true, false, "")
	}
	settledAt := observed.Add(3 * time.Minute)
	state = load(scope)
	apply(state, p1, false, false, "")
	apply(state, p3, false, false, "")
	if messages, err := state.archiveSettledATFragments(settledAt); err != nil || len(messages) != 0 {
		t.Fatalf("archived a cross-SCTS SMS with an unobserved part: %+v, %v", messages, err)
	}
	state = load(scope)
	for _, part := range []atInboxPart{p1, p2, p3} {
		apply(state, part, false, false, "")
	}
	messages, err = state.archiveSettledATFragments(settledAt)
	if err != nil || len(messages) != 1 || messages[0].Content != "全部在" {
		t.Fatalf("archive with all parts observed: %+v, %v", messages, err)
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

func TestATStoredSMSNoticeSurvivesReadFailure(t *testing.T) {
	w := &Worker{}
	slot := storedSMSSlot{store: "ME", index: 7}
	// With identity still unknown, a notification cannot attest a later card.
	w.handleStoredSMS(slot.store, "7")
	if len(w.storedSMSNotices.slots) != 0 {
		t.Fatal("unknown SIM claimed an ME slot")
	}
	owner := storedSMSOwner{simGeneration: 1}
	notice := &storedSMSNotice{owner: owner, slot: slot, observedAt: time.Now()}
	w.storedSMSNotices.add(notice) // An unsuccessful CMGR leaves the notification unresolved.

	retry := w.storedSMSNotices.snapshot(owner, slot.store)[slot.index]
	if retry != notice || !w.storedSMSNotices.bind(retry, "001122") {
		t.Fatal("periodic list could not recover the pending ME notification")
	}
	if w.storedSMSNotices.bind(retry, "aabbcc") {
		t.Fatal("retry accepted a replacement after the original PDU was read")
	}

	next := &storedSMSNotice{owner: owner, slot: slot, observedAt: time.Now()}
	w.storedSMSNotices.add(next)
	w.storedSMSNotices.forget(notice)
	if !w.storedSMSNotices.bind(next, "aabbcc") {
		t.Fatal("old task discarded a newer slot notification")
	}
	owner.simGeneration++
	if len(w.storedSMSNotices.snapshot(owner, slot.store)) != 0 || w.storedSMSNotices.bind(next, "aabbcc") {
		t.Fatal("new SIM generation inherited an ME notice")
	}
	latest := &storedSMSNotice{owner: owner, slot: slot, observedAt: time.Now()}
	w.storedSMSNotices.add(latest)
	if len(w.storedSMSNotices.snapshot(next.owner, slot.store)) != 0 || !w.storedSMSNotices.current(latest) {
		t.Fatal("an old scan discarded a newer SIM generation's notice")
	}
}

func TestATStoredSMSDuplicateRequiresReceiptIMSIForDeletion(t *testing.T) {
	initDevicePhoneNumberTestDB(t)
	identity := SMSIdentity{ICCID: "8988110000000000001", IMSI: "001010123456789"}
	part, err := decodeATInboxPart(identity, "0004038177F7000862119121000000027532")
	if err != nil || part.Concat.IsConcat || part.Content != "甲" {
		t.Fatalf("single PDU fixture = %+v, %v", part, err)
	}
	scope := db.SMSInboundFragmentScope{DeviceID: "c", ICCID: identity.ICCID, IMSI: identity.IMSI}
	original, err := loadATSMSParts(scope)
	if err != nil {
		t.Fatal(err)
	}
	if stored, inbound, err := original.persist(part, true); err != nil || !stored || inbound == nil {
		t.Fatalf("initial persist = %t, %+v, %v", stored, inbound, err)
	}
	scope.IMSI = "001010123456788"
	changed, err := loadATSMSParts(scope)
	if err != nil {
		t.Fatal(err)
	}
	assertRetry := func(state *atSMSParts, sourceKnown, wantDelete bool) {
		t.Helper()
		if stored, inbound, err := state.persist(part, sourceKnown); err != nil || stored != wantDelete || inbound != nil {
			t.Fatalf("replay persist = %t, %+v, %v, want delete=%t", stored, inbound, err, wantDelete)
		}
	}
	assertRetry(changed, true, false)
	assertRetry(original, true, true)

	var row db.SMS
	if err := db.DB.Where("iccid = ? AND fragment_session_key = ?", identity.ICCID, part.key).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := db.DeleteSMSByID(row.ID); err != nil {
		t.Fatal(err)
	}
	assertRetry(changed, true, false)
	assertRetry(changed, false, false)
	assertRetry(original, true, true)
	assertRetry(original, false, true)
	var count int64
	if err := db.DB.Model(&db.SMS{}).Where("iccid = ?", identity.ICCID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("replay recreated removed history: count=%d, err=%v", count, err)
	}
}

type storedSMSIdentityTestReader struct {
	SMSIdentity
	onIMSI func()
}

func (r storedSMSIdentityTestReader) QuerySIMInserted() (bool, error) { return true, nil }
func (r storedSMSIdentityTestReader) QueryICCID() (string, error)     { return r.ICCID, nil }
func (r storedSMSIdentityTestReader) QueryIMSI() (string, error) {
	if r.onIMSI != nil {
		r.onIMSI()
	}
	return r.IMSI, nil
}

func TestATStoredSMSLiveSwapSchedulesSIMConvergence(t *testing.T) {
	p, w, be := newML307CIdentityWorker(t)
	configureWorkerATSMS(w)
	owner, err := w.storedSMSOwner()
	if err != nil {
		t.Fatal(err)
	}
	live := SMSIdentity{ICCID: "card-new", IMSI: "imsi-new"}
	be.liveICCID, be.liveIMSI = live.ICCID, live.IMSI
	if err := owner.verifyLiveFrom(storedSMSIdentityTestReader{SMSIdentity: live}); !errors.Is(err, ErrSMSIdentityConflict) {
		t.Fatalf("live swap returned %v", err)
	}
	if !w.SIMIdentityConvergenceMatches("", 0) || w.SIMIdentityGeneration() != owner.simGeneration+1 {
		t.Fatal("live swap left old cached identity ready")
	}
	var messages []InboundSMS
	if err := w.readStoredSMSInbox(owner, nil, &messages); !errors.Is(err, ErrSMSIdentityTransitioning) || len(messages) != 0 {
		t.Fatalf("old owner could enter the inbox: messages=%+v, err=%v", messages, err)
	}
	p.simEventMu.Lock()
	timer := p.simEventTimers[w.ID]
	p.simEventMu.Unlock()
	if timer == nil || !timer.Stop() {
		t.Fatal("inbox mismatch did not schedule SIM status convergence")
	}
	p.confirmSIMRemovedForOwner(owner.workerLifecycleOwner, "test", timer)
	current, err := w.storedSMSOwner()
	if err != nil || current.SMSIdentity != live || current.simGeneration <= owner.simGeneration {
		t.Fatalf("inbox did not converge to the live identity: %+v, %v", current.SMSIdentity, err)
	}
}

func TestATStoredSMSStaleLiveReadLeavesNewerSIMReady(t *testing.T) {
	p, w, _ := newML307CIdentityWorker(t)
	configureWorkerATSMS(w)
	owner, err := w.storedSMSOwner()
	if err != nil {
		t.Fatal(err)
	}
	live := SMSIdentity{ICCID: "card-new", IMSI: "imsi-new"}
	reader := storedSMSIdentityTestReader{
		SMSIdentity: SMSIdentity{ICCID: "card-intermediate", IMSI: "imsi-intermediate"},
		onIMSI: func() {
			w.cacheMu.Lock()
			defer w.cacheMu.Unlock()
			setWorkerSMSIdentity(w, live)
			w.state.Identity.Generation++
		},
	}
	if err := owner.verifyLiveFrom(reader); !errors.Is(err, ErrSMSIdentityTransitioning) {
		t.Fatalf("superseded read returned %v", err)
	}
	current, err := w.storedSMSOwner()
	if err != nil || current.SMSIdentity != live || current.simGeneration != owner.simGeneration+1 {
		t.Fatalf("superseded read altered the newer generation: %+v, %v", current.SMSIdentity, err)
	}
	p.simEventMu.Lock()
	timer := p.simEventTimers[w.ID]
	p.simEventMu.Unlock()
	if timer != nil {
		t.Fatal("superseded read scheduled a new SIM status check")
	}
}
