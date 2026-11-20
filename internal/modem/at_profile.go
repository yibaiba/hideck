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

type atTextQuery struct {
	command string
	parse   func(string) string
}

type atRadioQuery struct {
	command string
	parse   func(string) (mode, duplex, band string, channel uint32)
}

type atCPINPresenceQuery struct {
	parse         func(string) (bool, error)
	isAbsentError func(string) bool
}

type atOwnedSMSSubmit struct {
	promptTimeout time.Duration
	finalTimeout  time.Duration
}

type atDialect struct {
	imei               atTextQuery
	firmware           atTextQuery
	iccid              atTextQuery
	parseProbeIMEI     func(string) string
	missingIMEIError   error
	operatorFormat     string
	registration       string
	registrationPrefix string
	radio              atRadioQuery
	radioFallback      atRadioQuery
	servingCell        string
	imsStatus          string
	usbnet             string
	simStatus          string
	cpinPresence       *atCPINPresenceQuery
	parseCCHO          func(string) (int, bool)
	offlineSIMReload   bool
	voiceInit          []string
	ownedSMSSubmit     *atOwnedSMSSubmit
}

var defaultATDialect = atDialect{
	imei:               atTextQuery{"AT+CGSN", parseIMEI},
	firmware:           atTextQuery{"AT+CGMR", parseFirmware},
	iccid:              atTextQuery{"AT+QCCID", parseQCCID},
	parseProbeIMEI:     parseIMEI,
	operatorFormat:     "AT+COPS=3,2",
	registration:       "AT+CREG?",
	registrationPrefix: "+CREG:",
	radio:              atRadioQuery{"AT+QNWINFO", parseQNWInfoRadio},
	radioFallback:      atRadioQuery{"AT+COPS?", parseCOPSRadio},
	servingCell:        "AT+QENG=\"servingcell\"",
	imsStatus:          "AT+QIMS?",
	usbnet:             "AT+QCFG=\"usbnet\"?",
	simStatus:          "AT+QSIMSTAT?",
	parseCCHO:          parseCCHO,
	voiceInit:          []string{"AT+CLIP=1", "AT+QPCMV=1,2"},
}

var ml307cATDialect = atDialect{
	imei:               atTextQuery{"AT+CGSN=1", parseML307CIMEI},
	firmware:           atTextQuery{"AT+MSWVER", parseML307CFirmware},
	iccid:              atTextQuery{"AT+MCCID", parseMCCID},
	parseProbeIMEI:     parseML307CProbeIMEI,
	missingIMEIError:   errors.New("invalid IMEI response"),
	registration:       "AT+CEREG?",
	registrationPrefix: "+CEREG:",
	radio:              atRadioQuery{"AT+COPS?", parseCOPSRadio},
	cpinPresence:       &atCPINPresenceQuery{parse: parseML307CCPIN, isAbsentError: isML307CNoSIMError},
	parseCCHO:          parseML307CCCHO,
	offlineSIMReload:   true,
	ownedSMSSubmit:     &atOwnedSMSSubmit{promptTimeout: ml307cSMSPromptTimeout, finalTimeout: ml307cSMSResultTimeout},
}

func (profile ATProfile) dialect() *atDialect {
	if profile == ATProfileML307C {
		return &ml307cATDialect
	}
	return &defaultATDialect
}

// SupportsOfflineSIMReload reports whether the SIM can reload without enabling RF.
func (profile ATProfile) SupportsOfflineSIMReload() bool {
	return profile.dialect().offlineSIMReload
}

func parseML307CIMEI(resp string) string {
	if line, ok := findLineWithPrefix(resp, "+CGSN:"); ok {
		return parseIMEI(strings.TrimPrefix(line, "+CGSN:"))
	}
	return parseIMEI(resp)
}

func parseML307CProbeIMEI(resp string) string {
	// A serial read can end in the middle of an identifier.
	return parseML307CIMEI(resp[:strings.LastIndexAny(resp, "\r\n")+1])
}

func parseML307CFirmware(resp string) string {
	// MSWVER reports model, firmware version and build on separate lines.
	var fields []string
	for _, line := range splitLines(resp) {
		if line == "" || line == "OK" || line == "AT+MSWVER" || strings.HasPrefix(line, "+") {
			continue
		}
		fields = append(fields, line)
	}
	if len(fields) != 3 {
		return ""
	}
	return fields[1] + " / " + fields[2]
}

func parseMCCID(resp string) string {
	return parseICCID(resp, "+MCCID:")
}

func parseCOPSRadio(resp string) (string, string, string, uint32) {
	mode, _ := parseCOPSAct(resp)
	return mode, "", "", 0
}

const (
	cpinQuery  = "AT+CPIN?"
	cpinPrefix = "+CPIN:"
)

var (
	errSIMNotInserted = errors.New("SIM not inserted")
	ErrSIMPINRequired = errors.New("SIM PIN or PUK required")
)

func parseML307CCPIN(resp string) (bool, error) {
	line, ok := findLineWithPrefix(resp, cpinPrefix)
	if !ok {
		return false, errors.New("unrecognized CPIN response")
	}
	state := strings.TrimSpace(strings.TrimPrefix(line, cpinPrefix))
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

func (m *Manager) queryCPINSIMPresence(query *atCPINPresenceQuery) (bool, error) {
	resp, err := m.ExecuteATSilent(cpinQuery, 2*time.Second)
	if errors.Is(err, errSIMNotInserted) {
		m.cacheCPINSIMState(false, nil)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	inserted, err := query.parse(resp)
	if err == nil || errors.Is(err, ErrSIMPINRequired) {
		m.cacheCPINSIMState(inserted, err)
	}
	return inserted, err
}

func (m *Manager) cacheCPINSIMState(inserted bool, err error) {
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
