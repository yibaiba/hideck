package modem

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// SMSInbox keeps one AT receive transaction on a verified CPMS read store.
type SMSInbox struct {
	modem   *Manager
	current []string
	dirty   bool
}

func (m *Manager) SetStoredSMSHandler(handler func(storage, index string)) {
	m.infoMu.Lock()
	defer m.infoMu.Unlock()
	m.storedSMSHandler = handler
}

func (m *Manager) StoredSMSReceiving() bool {
	m.infoMu.RLock()
	defer m.infoMu.RUnlock()
	return m.storedSMSHandler != nil && !m.disableURCRead
}

// WithSMSInbox shares the storage lock with PDU sending. The callback may
// select both stores before the original three CPMS locations are restored.
func (m *Manager) WithSMSInbox(run func(*SMSInbox) error) (err error) {
	m.smsStorageMu.Lock()
	defer m.smsStorageMu.Unlock()
	if !m.StoredSMSReceiving() {
		return errors.New("AT SMS receive is paused")
	}
	initial, err := m.smsStorageSelection()
	if err != nil {
		return err
	}
	inbox := &SMSInbox{modem: m, current: initial}
	defer func() {
		if inbox.dirty {
			err = errors.Join(err, m.setAndVerifySMSStorage(initial))
		}
	}()
	return run(inbox)
}

func (m *Manager) smsStorageSelection() ([]string, error) {
	reply, err := m.ExecuteATSilent("AT+CPMS?", 3*time.Second)
	if err != nil {
		return nil, err
	}
	selection := parseCPMSStorages(reply)
	if len(selection) != 3 {
		return nil, errors.New("cannot verify all three CPMS locations")
	}
	return selection, nil
}

func (m *Manager) setAndVerifySMSStorage(selection []string) error {
	if _, err := m.ExecuteATSilent(cpmsSetCommand(selection...), 5*time.Second); err != nil {
		return err
	}
	return m.verifySMSStorage(selection)
}

func (m *Manager) verifySMSStorage(selection []string) error {
	actual, err := m.smsStorageSelection()
	if err != nil {
		return err
	}
	if !slices.Equal(actual, selection) {
		return fmt.Errorf("CPMS selected %v, expected %v", actual, selection)
	}
	return nil
}

func (inbox *SMSInbox) Select(storage string) error {
	storage = strings.ToUpper(strings.TrimSpace(storage))
	if storage != "SM" && storage != "ME" {
		return fmt.Errorf("unsupported SMS read store %q", storage)
	}
	if inbox.current == nil {
		// A failed CPMS may still have changed one or more locations.
		actual, err := inbox.modem.smsStorageSelection()
		if err != nil {
			return err
		}
		inbox.current = actual
	}
	if inbox.current[0] == storage {
		return nil
	}
	// ML307C accepts the same store for all three locations.
	selection := []string{storage, storage, storage}
	inbox.dirty = true
	inbox.current = nil
	if err := inbox.modem.setAndVerifySMSStorage(selection); err != nil {
		return err
	}
	inbox.current = selection
	return nil
}

func (inbox *SMSInbox) List() ([]SMSPDURecord, error) {
	return inbox.modem.smsListAllPDURecords(true)
}

func (inbox *SMSInbox) Read(index int) (SMSPDURecord, error) {
	reply, err := inbox.modem.ExecuteATSilent("AT+CMGR="+strconv.Itoa(index), 5*time.Second)
	if err != nil {
		return SMSPDURecord{}, err
	}
	header, _ := findLineWithPrefix(reply, "+CMGR:")
	fields := parseCommaFields(parseURCAfterColon(header))
	pdu, ok := extractSMSPDUAfterPrefix(reply, "+CMGR:")
	if !ok || len(fields) == 0 {
		return SMSPDURecord{}, errors.New("incomplete CMGR record")
	}
	status, err := strconv.Atoi(fields[0])
	if err != nil {
		return SMSPDURecord{}, errors.New("invalid CMGR status")
	}
	return SMSPDURecord{Index: index, Status: status, PDU: pdu}, nil
}

// DeleteMatching confirms that the selected store and slot still match the
// committed PDU. check runs just before the delete to recheck SIM ownership.
func (inbox *SMSInbox) DeleteMatching(record SMSPDURecord, check func() error) error {
	if err := inbox.modem.verifySMSStorage(inbox.current); err != nil {
		return err
	}
	current, err := inbox.Read(record.Index)
	if err != nil {
		return err
	}
	if !current.Received() || !strings.EqualFold(current.PDU, record.PDU) {
		return errors.New("SMS slot changed before deletion")
	}
	if err := check(); err != nil {
		return err
	}
	return inbox.modem.DeleteSMS(uint32(record.Index))
}
