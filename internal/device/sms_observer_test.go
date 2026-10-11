package device

import (
	"errors"
	"testing"
	"time"

	"github.com/yibaiba/hideck/internal/db"
)

type observerSMSStore struct {
	err error
}

func (s observerSMSStore) LookupDeviceIdentity(db.SMSDeviceSelector) (SMSIdentity, bool, error) {
	return SMSIdentity{}, false, nil
}

func (s observerSMSStore) SaveReceived(SMSIdentity, inboundSMSRecord) error {
	return s.err
}

func TestInboundSMSObserverReceivesCanonicalStoredMessage(t *testing.T) {
	p := NewPool(nil)
	var received []InboundSMS
	p.OnInboundSMS(func(message InboundSMS) error {
		received = append(received, message)
		return nil
	})
	at := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	p.notifyInboundSMS(InboundSMS{DeviceID: "wwan0", ICCID: "iccid", Sender: "giffgaff", Content: "credit 1.00", Time: at})
	if len(received) != 1 || received[0].Content != "credit 1.00" || !received[0].Time.Equal(at) {
		t.Fatalf("received = %+v", received)
	}
}

func TestInboundSMSObserverIgnoresMissingIdentity(t *testing.T) {
	p := NewPool(nil)
	called := false
	p.OnInboundSMS(func(InboundSMS) error { called = true; return nil })
	p.notifyInboundSMS(InboundSMS{DeviceID: "wwan0", Content: "message"})
	if called {
		t.Fatal("observer called without ICCID")
	}
}

func TestCellularSMSNotifiesObserverOnlyAfterPersistence(t *testing.T) {
	p := NewPool(nil)
	p.smsIdentities = observerSMSStore{}
	worker := &Worker{ID: "wwan0", Pool: p}
	called := 0
	p.OnInboundSMS(func(InboundSMS) error { called++; return nil })
	identity := SMSIdentity{ICCID: "iccid", IMSI: "imsi"}
	message := inboundSMSRecord{Sender: "giffgaff", Content: "credit 1.00", Timestamp: time.Now()}
	if err := worker.processSMSWithIdentity(identity, message); err != nil {
		t.Fatalf("processSMSWithIdentity() error = %v", err)
	}
	if called != 1 {
		t.Fatalf("observer calls = %d, want 1", called)
	}

	p.smsIdentities = observerSMSStore{err: errors.New("write failed")}
	if err := worker.processSMSWithIdentity(identity, message); err == nil {
		t.Fatal("processSMSWithIdentity() succeeded after persistence failure")
	}
	if called != 1 {
		t.Fatalf("observer called after persistence failure, calls = %d", called)
	}
}
