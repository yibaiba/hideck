package device

import (
	"encoding/hex"
	"testing"

	"github.com/yibaiba/hideck/internal/db"
)

func TestML307CStoredSMSPersistence(t *testing.T) {
	initDevicePhoneNumberTestDB(t)
	identity := SMSIdentity{ICCID: "8988110000000000001", IMSI: "001010123456789"}
	scope := db.SMSInboundFragmentScope{DeviceID: "c", ICCID: identity.ICCID, IMSI: identity.IMSI}
	fragment := func(seq, second byte, ch rune) atInboxPart {
		t.Helper()
		pdu := []byte{
			0,                         // default SMSC
			0x40, 3, 0x81, 0x77, 0xF7, // SMS-DELIVER, UDHI, sender 777
			0, 8, // PID, UCS2
			0x62, 0x11, 0x91, 0x21, 0, second, 0, // SCTS
			8, 5, 0, 3, 17, 2, seq, byte(ch >> 8), byte(ch),
		}
		part, err := decodeATInboxPart(identity, hex.EncodeToString(pdu))
		if err != nil {
			t.Fatal(err)
		}
		if !part.Concat.IsConcat || part.Sender != "777" || part.Content != string(ch) {
			t.Fatalf("unexpected decoded part: %+v", part)
		}
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

	a1, a2 := fragment(1, 0, '甲'), fragment(2, 0, '乙')
	b1, b2 := fragment(1, 0x10, '丙'), fragment(2, 0x10, '丁')
	state := load(scope)
	apply(state, a1, false, false, "") // unknown ME: no history and no delete
	apply(state, a1, true, true, "")   // CMTI persists the first part before removing its slot

	state = load(scope) // new process/worker can see the durable fragment
	if state.groups[a1.key][0].Content != "甲" {
		t.Fatal("first fragment was not restored")
	}
	apply(state, b2, true, true, "")    // same ref, different SCTS cannot complete A
	apply(state, a2, false, true, "甲乙") // the missing tail arrived in ME while HiDeck was offline
	apply(state, b1, true, true, "丙丁")

	state = load(scope)               // deletion failed before restart; the PDU is still on ME
	apply(state, a2, false, true, "") // matched durable part, no duplicate notification
	if err := state.clearCompletedGroups(); err != nil {
		t.Fatal(err)
	}
	if len(load(scope).groups[a1.key]) != 2 {
		t.Fatal("a scan containing a residual slot retired its fragment rows")
	}
	otherDevice := scope
	otherDevice.DeviceID = "c-other"
	if err := load(otherDevice).clearCompletedGroups(); err != nil {
		t.Fatal(err)
	}
	if len(load(scope).groups[a1.key]) != 2 {
		t.Fatal("another module's empty scan retired the original module's fragments")
	}
	if err := load(scope).clearCompletedGroups(); err != nil { // subsequent complete, empty scan
		t.Fatal(err)
	}
	if len(load(scope).groups) != 0 {
		t.Fatal("completed fragment groups were not collected")
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
