package modem

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFailedCLCCExposesPartialResponseWithoutLoggingNumbers(t *testing.T) {
	m := newRunningTestManager(t)
	m.port = &timeoutSerialPort{}
	req := commandRequest{cmd: "AT+CLCC", silent: true, timeout: time.Second, respChan: make(chan string, 1), errChan: make(chan error, 1)}
	const partial = "+CLCC: 1,1,5,0,0,\"447700900123\",145"
	go func() { m.rxChan <- rxMsg{Data: partial}; m.rxChan <- rxMsg{Data: "+CME ERROR: 100"} }()
	m.handleCommand(req)
	select {
	case err := <-req.errChan:
		var response interface{ PartialATResponse() string }
		if !errors.As(err, &response) || !strings.Contains(response.PartialATResponse(), partial) {
			t.Fatalf("missing partial snapshot: %v", err)
		}
		if strings.Contains(err.Error(), "447700900123") {
			t.Fatal("error exposes call number")
		}
	case <-time.After(time.Second):
		t.Fatal("CLCC failure did not complete")
	}
}
