package modem

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/yibaiba/hideck/internal/config"
)

func TestML307CATQueries(t *testing.T) {
	m, err := NewWithATProfile(config.DeviceConfig{ID: "c", DeviceBackend: "at", ATPort: "/dev/ttyUSB8"}, ATProfileML307C)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.initializationCommands(); !slices.Equal(got, []string{"ATE0", "AT+CMGF=0", "AT+CNMI=2,1,0,0,0"}) {
		t.Fatalf("init commands = %v", got)
	}

	fixtures := map[string][]string{
		"AT+CGSN=1": {"+CGSN: 490154203237518", "OK"},
		"AT+MCCID":  {"+MCCID: 8988110000000000001F", "OK"},
		"AT+MSWVER": {"ML307C-DC-CN-MBRH0S00", "4.0.8.2506051839_release", "6c31ce603", "OK"},
		"AT+CEREG?": {"+CEREG: 0,5", "OK"},
		"AT+COPS?":  {"+COPS: 0,0,\"CHN-UNICOM\",7", "OK"},
		"AT+CSQ":    {"+CSQ: 24,99", "OK"},
	}
	var pinResponses = [][]string{
		{"+CPIN: READY", "OK"}, {"+CPIN: SIM PIN", "OK"}, {"+CME ERROR: SIM not inserted"}, {"ERROR"},
	}
	m.port = &timeoutSerialPort{}
	m.setRunning(true)
	t.Cleanup(m.Stop)
	go func() {
		for {
			select {
			case <-m.stop:
				return
			case req := <-m.cmdChan:
				lines := fixtures[req.cmd]
				if req.cmd == "AT+CPIN?" && len(pinResponses) > 0 {
					lines, pinResponses = pinResponses[0], pinResponses[1:]
				}
				if len(lines) == 0 {
					req.errChan <- errors.New("unexpected AT command: " + req.cmd)
					continue
				}
				for _, line := range lines {
					m.rxChan <- rxMsg{Data: line}
				}
				m.handleCommand(req)
			}
		}
	}()

	if imei, err := m.QueryIMEI(); err != nil || imei != "490154203237518" {
		t.Fatalf("QueryIMEI() = %q, %v", imei, err)
	}
	if firmware, err := m.QueryFirmware(); err != nil || firmware != "4.0.8.2506051839_release / 6c31ce603" {
		t.Fatalf("QueryFirmware() = %q, %v", firmware, err)
	}
	if iccid, err := m.QueryICCID(); err != nil || iccid != "8988110000000000001" {
		t.Fatalf("QueryICCID() = %q, %v", iccid, err)
	}
	if status, _, _, _, err := m.QueryRegistration(); err != nil || status != 5 {
		t.Fatalf("QueryRegistration() = %d, %v", status, err)
	}
	if operator, err := m.QueryOperator(); err != nil || operator != "CHN-UNICOM" {
		t.Fatalf("QueryOperator() = %q, %v", operator, err)
	}
	if rat, _, _, _, err := m.QueryNetworkRadio(); err != nil || rat != "LTE" {
		t.Fatalf("QueryNetworkRadio() = %q, %v", rat, err)
	}
	if _, rssi, err := m.QueryCSQ(); err != nil || rssi != -65 {
		t.Fatalf("QueryCSQ() RSSI = %d, %v", rssi, err)
	}
	if ready, err := m.QuerySIMInserted(); !ready || err != nil || !m.IsSimInserted() {
		t.Fatalf("ready CPIN = %v, %v", ready, err)
	}
	simEvents := make(chan string, 2)
	m.SetSIMStatusHandler(func(_ *bool, state string) { simEvents <- state })
	m.handleURC("+CPIN: SIM PIN")
	select {
	case state := <-simEvents:
		if state != "SIM PIN" || !m.IsSimInserted() {
			t.Fatalf("PIN URC changed confirmed readiness: event=%q ready=%v", state, m.IsSimInserted())
		}
	case <-time.After(time.Second):
		t.Fatal("PIN URC was not dispatched")
	}
	if ready, err := m.QuerySIMInserted(); ready || !errors.Is(err, ErrSIMPINRequired) || m.IsSimInserted() {
		t.Fatalf("locked CPIN = %v, %v", ready, err)
	}
	select {
	case state := <-simEvents:
		t.Fatalf("live PIN query requeued a SIM callback: %q", state)
	case <-time.After(50 * time.Millisecond):
	}
	if ready, err := m.QuerySIMInserted(); ready || err != nil {
		t.Fatalf("absent CPIN = %v, %v", ready, err)
	}
	if ready, err := m.QuerySIMInserted(); ready || err == nil || !m.IsHealthy() {
		t.Fatalf("failed CPIN = %v, %v, healthy=%v", ready, err, m.IsHealthy())
	}
}

func TestML307CFirmwareParsingRejectsIncompleteReply(t *testing.T) {
	if got := ATProfileML307C.dialect().firmware.parse("AT+MSWVER\nML307C-DC-CN-MBRH0S00\n+CPIN: READY\nOK"); got != "" {
		t.Fatalf("incomplete MSWVER reply parsed as %q", got)
	}
	if got := ATProfileDefault.dialect().firmware.parse("EC20EFAR06A05M4G\nOK"); got != "EC20EFAR06A05M4G" {
		t.Fatalf("default firmware parsed as %q", got)
	}
}

func TestML307CIMEIParsing(t *testing.T) {
	profile := ATProfileML307C
	for _, invalid := range []string{"20200X000000000000", "+CGSN: 4901542032375180", "+CGSN: 490154203237518,0"} {
		if imei := profile.dialect().imei.parse(invalid); imei != "" {
			t.Fatalf("invalid CGSN response %q parsed as %q", invalid, imei)
		}
	}
	if imei := profile.dialect().imei.parse("490154203237518"); imei != "490154203237518" {
		t.Fatalf("bare IMEI parsed as %q", imei)
	}
}
