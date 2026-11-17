package modemvoice

import (
	"encoding/csv"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var errCLCCRejectedSnapshot = errors.New("modem voice: CLCC snapshot ended in device error")

// State values follow Quectel EC25/EC21 AT manual section 7.15 (TS 27.007).
type State int

const (
	Active State = iota
	Held
	Dialing
	Alerting
	Incoming
	Waiting
)

type Call struct {
	Index      int
	Inbound    bool
	State      State
	Mode       int // 0 voice; data/fax/unknown entries remain visible in snapshots.
	Multiparty bool
	Number     string
	NumberType int
}

// ParseCLCC accepts the response body of a successful AT+CLCC transaction.
// Transport errors must never be passed as an empty response.
func ParseCLCC(response string) ([]Call, error) {
	calls := make([]Call, 0)
	seen := make(map[int]bool)
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(response, "\r", "")), "\n")
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || line == "OK" || line == "AT+CLCC" {
			continue
		}
		if len(calls) > 0 && i == len(lines)-1 && (line == "ERROR" || strings.HasPrefix(line, "+CME ERROR:")) {
			return nil, errCLCCRejectedSnapshot
		}
		if !strings.HasPrefix(line, "+CLCC:") {
			return nil, fmt.Errorf("modem voice: unexpected CLCC response line")
		}
		call, err := parseCall(strings.TrimSpace(strings.TrimPrefix(line, "+CLCC:")))
		if err != nil {
			return nil, err
		}
		if seen[call.Index] {
			return nil, fmt.Errorf("modem voice: duplicate CLCC index %d", call.Index)
		}
		seen[call.Index] = true
		calls = append(calls, call)
	}
	return calls, nil
}

func parseCall(line string) (Call, error) {
	r := csv.NewReader(strings.NewReader(line))
	r.TrimLeadingSpace = true
	fields, err := r.Read()
	if err != nil || (len(fields) != 5 && len(fields) != 7 && len(fields) != 8) {
		return Call{}, fmt.Errorf("modem voice: malformed CLCC fields")
	}
	values := make([]int, 5)
	for i := range values {
		value, err := strconv.Atoi(strings.TrimSpace(fields[i]))
		if err != nil {
			return Call{}, fmt.Errorf("modem voice: invalid CLCC field %d", i)
		}
		values[i] = value
	}
	if values[0] <= 0 || values[1] < 0 || values[1] > 1 || values[2] < int(Active) || values[2] > int(Waiting) ||
		(values[3] != 0 && values[3] != 1 && values[3] != 2 && values[3] != 9) || values[4] < 0 || values[4] > 1 {
		return Call{}, fmt.Errorf("modem voice: invalid CLCC values")
	}
	call := Call{Index: values[0], Inbound: values[1] == 1, State: State(values[2]), Mode: values[3], Multiparty: values[4] == 1}
	if len(fields) >= 7 {
		call.NumberType, err = strconv.Atoi(strings.TrimSpace(fields[6]))
		if err != nil || call.NumberType < 0 || call.NumberType > 255 {
			return Call{}, fmt.Errorf("modem voice: invalid CLCC number type")
		}
		call.Number = fields[5]
		if call.NumberType == 145 && call.Number != "" && !strings.HasPrefix(call.Number, "+") {
			call.Number = "+" + call.Number
		}
	}
	return call, nil
}
