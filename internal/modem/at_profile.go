package modem

import (
	"errors"
	"strings"
	"time"
)

type ATProfile uint8

const (
	ATProfileDefault ATProfile = iota
	ATProfileML307C
)

func (p ATProfile) imeiCommand() string {
	if p == ATProfileML307C {
		return "AT+CGSN=1"
	}
	return "AT+CGSN"
}

func (p ATProfile) parseIMEI(resp string) string {
	if p == ATProfileML307C {
		if line, ok := findLineWithPrefix(resp, "+CGSN:"); ok {
			return parseIMEI(strings.TrimPrefix(line, "+CGSN:"))
		}
	}
	return parseIMEI(resp)
}

func (p ATProfile) iccidCommand() string {
	if p == ATProfileML307C {
		return "AT+MCCID"
	}
	return "AT+QCCID"
}

func (p ATProfile) parseICCID(resp string) string {
	if p == ATProfileML307C {
		return parseICCID(resp, "+MCCID:")
	}
	return parseQCCID(resp)
}

var (
	errSIMNotInserted = errors.New("SIM not inserted")
	ErrSIMPINRequired = errors.New("SIM PIN or PUK required")
)

func parseML307CCPIN(resp string) (bool, error) {
	line, ok := findLineWithPrefix(resp, "+CPIN:")
	if !ok {
		return false, errors.New("unrecognized CPIN response")
	}
	state := strings.TrimSpace(strings.TrimPrefix(line, "+CPIN:"))
	if IsSIMPINRequiredState(state) {
		return false, ErrSIMPINRequired
	}
	switch strings.ToUpper(state) {
	case "READY":
		return true, nil
	case "SIM REMOVED", "SIM NOT INSERTED", "NOT INSERTED":
		return false, nil
	default:
		return false, errors.New("unrecognized CPIN state")
	}
}

// IsSIMPINRequiredState recognizes CPIN states where the SIM is present and locked.
func IsSIMPINRequiredState(state string) bool {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "SIM PIN", "SIM PUK", "SIM PIN2", "SIM PUK2":
		return true
	default:
		return false
	}
}

func (m *Manager) queryML307CSIMInserted() (bool, error) {
	resp, err := m.ExecuteATSilent("AT+CPIN?", 2*time.Second)
	if errors.Is(err, errSIMNotInserted) {
		m.cacheML307CSIMState(false, nil)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	inserted, err := parseML307CCPIN(resp)
	if err == nil || errors.Is(err, ErrSIMPINRequired) {
		m.cacheML307CSIMState(inserted, err)
	}
	if errors.Is(err, ErrSIMPINRequired) {
		m.dispatchSIMStatusURC(nil, "SIM PIN")
	}
	return inserted, err
}

func (m *Manager) cacheML307CSIMState(inserted bool, err error) {
	m.infoMu.Lock()
	defer m.infoMu.Unlock()
	m.simInserted = inserted
	if !inserted && err == nil {
		m.iccid = ""
		m.imsi = ""
	}
}

func isML307CNoSIMError(line string) bool {
	value, ok := strings.CutPrefix(line, "+CME ERROR:")
	if !ok {
		return false
	}
	value = strings.TrimSpace(value)
	return value == "10" || strings.EqualFold(value, "SIM not inserted")
}
