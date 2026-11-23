package modem

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestHandleCMTIUsesIndicatedStorageForReadAndDelete(t *testing.T) {
	m := newRunningTestManager(t)
	validPDU := "079144872000302320048102020000625061028204401AD9775D0E72D7DBE2B21C949E8360B75A4E7683D16AB71B"

	done := make(chan []string, 1)
	go func() {
		done <- respondToCommands(t, m, 5, func(req commandRequest) {
			switch req.cmd {
			case "AT+CPMS?":
				req.respChan <- "\r\n+CPMS: \"SM\",0,10,\"SM\",0,10,\"SM\",0,10\r\n\r\nOK\r\n"
			case `AT+CPMS="ME","ME","ME"`:
				req.respChan <- "OK"
			case "AT+CMGR=7":
				req.respChan <- "\r\n+CMGR: 0,,38\r\n" + validPDU + "\r\n\r\nOK\r\n"
			case "AT+CMGD=7":
				req.respChan <- "OK"
			case `AT+CPMS="SM","SM","SM"`:
				req.respChan <- "OK"
			default:
				req.errChan <- nil
			}
		})
	}()

	m.handleURC(`+CMTI: "ME",7`)

	var got []string
	select {
	case got = <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for storage-aware CMTI handling")
	}
	want := []string{
		"AT+CPMS?",
		`AT+CPMS="ME","ME","ME"`,
		"AT+CMGR=7",
		"AT+CMGD=7",
		`AT+CPMS="SM","SM","SM"`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commands=%#v want %#v", got, want)
	}
}

func TestML307CSMSInboxRestoresStorageAfterDeleteFailure(t *testing.T) {
	m := newRunningTestManager(t)
	m.SetStoredSMSHandler(func(_, _ string) {})
	m.atProfile = ATProfileML307C
	const pdu = "0004038101F100006250724190410A3754747A0E4ABBCD6F793B4C4FBFDDA0F41CE47ED341617B38CD0E8BD96590F92D07E5DF7539283C1EBFEB6E3A889E87971B"
	deleteErr := errors.New("slot could not be deleted")
	done := make(chan []string, 1)
	go func() {
		selection := `+CPMS: "ME",1,180,"ME",1,180,"ME",1,180`
		done <- respondToCommands(t, m, 9, func(req commandRequest) {
			switch req.cmd {
			case "AT+CPMS?":
				req.respChan <- selection
			case `AT+CPMS="SM","SM","SM"`:
				selection = `+CPMS: "SM",2,10,"SM",2,10,"SM",2,10`
				req.respChan <- "OK"
			case "AT+CMGL=4":
				req.respChan <- "+CMGL: 7,0,,59\n" + pdu + "\n+CMGL: 8,3,,59\n" + pdu
			case "AT+CMGR=7":
				req.respChan <- "+CMGR: 1,,59\n" + pdu
			case "AT+CMGD=7":
				req.errChan <- deleteErr
			case `AT+CPMS="ME","ME","ME"`:
				selection = `+CPMS: "ME",1,180,"ME",1,180,"ME",1,180`
				req.respChan <- "OK"
			default:
				req.errChan <- errors.New("unexpected command " + req.cmd)
			}
		})
	}()

	err := m.WithSMSInbox(func(inbox *SMSInbox) error {
		if err := inbox.Select("SM"); err != nil {
			return err
		}
		records, err := inbox.List()
		if err != nil {
			return err
		}
		if len(records) != 2 || records[0].Index != 7 || !records[0].Received() || records[1].Received() {
			t.Fatalf("CMGL records = %+v", records)
		}
		return inbox.DeleteMatching(records[0], func() error { return nil })
	})
	if !errors.Is(err, deleteErr) {
		t.Fatalf("WithSMSInbox() error = %v, want delete failure", err)
	}
	want := []string{
		"AT+CPMS?", `AT+CPMS="SM","SM","SM"`, "AT+CPMS?", "AT+CMGL=4",
		"AT+CPMS?", "AT+CMGR=7", "AT+CMGD=7", `AT+CPMS="ME","ME","ME"`, "AT+CPMS?",
	}
	if got := <-done; !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
}

func TestHandleCommandDispatchesCMTIWithoutPollutingResponse(t *testing.T) {
	m := newRunningTestManager(t)
	m.port = &timeoutSerialPort{}
	received := make(chan string, 1)
	m.SetNewSMSHandler(func(index string) { received <- index })
	m.rxChan <- rxMsg{Data: `+CMTI: "SM",9`}
	m.rxChan <- rxMsg{Data: "+CSQ: 20,99"}
	m.rxChan <- rxMsg{Data: "OK"}

	req := commandRequest{
		cmd:      "AT+CSQ",
		respChan: make(chan string, 1),
		errChan:  make(chan error, 1),
		timeout:  time.Second,
	}
	m.handleCommand(req)

	select {
	case index := <-received:
		if index != "9" {
			t.Fatalf("CMTI index = %q, want 9", index)
		}
	case <-time.After(time.Second):
		t.Fatal("CMTI handler was not called")
	}
	if response := <-req.respChan; response != "+CSQ: 20,99" {
		t.Fatalf("response = %q, want CSQ only", response)
	}
}

func TestReadSMSRetainsMessageWhenIdentityNotReady(t *testing.T) {
	m := newRunningTestManager(t)
	checks := 0
	m.SetSMSReadinessCheck(func() error {
		checks++
		return errors.New("identity unknown")
	})

	m.ReadAndProcessSMS("7")

	if checks != 1 {
		t.Fatalf("readiness checks=%d, want 1", checks)
	}
	if len(m.cmdChan) != 0 {
		t.Fatalf("queued AT commands=%d, want no read or delete", len(m.cmdChan))
	}
}

func TestSetSMSCUsesSharedATQueueAndRejectsInvalidNumber(t *testing.T) {
	m := newRunningTestManager(t)
	done := make(chan []string, 1)
	go func() {
		done <- respondToCommands(t, m, 1, func(req commandRequest) {
			req.respChan <- "OK"
		})
	}()

	if err := m.SetSMSC("+447802002606"); err != nil {
		t.Fatalf("SetSMSC() error = %v", err)
	}
	commands := <-done
	if !reflect.DeepEqual(commands, []string{`AT+CSCA="+447802002606"`}) {
		t.Fatalf("commands = %v", commands)
	}
	if err := m.SetSMSC(`+44";AT+CFUN=1`); err == nil {
		t.Fatal("SetSMSC() accepted command injection characters")
	}
}

func TestReadSMSRetainsMessageWhenProcessorFails(t *testing.T) {
	m := newRunningTestManager(t)
	validPDU := "079144872000302320048102020000625061028204401AD9775D0E72D7DBE2B21C949E8360B75A4E7683D16AB71B"
	m.SetSMSProcessor(func(sender, content string, timestamp time.Time) error {
		return errors.New("database unavailable")
	})

	done := make(chan []string, 1)
	go func() {
		done <- respondToCommands(t, m, 1, func(req commandRequest) {
			req.respChan <- "\r\n+CMGR: 0,,38\r\n" + validPDU + "\r\n\r\nOK\r\n"
		})
	}()
	m.ReadAndProcessSMS("7")
	commands := <-done

	if !reflect.DeepEqual(commands, []string{"AT+CMGR=7"}) {
		t.Fatalf("commands=%v, want read without delete", commands)
	}
}

func TestML307CSMSInboxRecoversOtherStoreAfterSelectionFailure(t *testing.T) {
	m := newRunningTestManager(t)
	m.SetStoredSMSHandler(func(_, _ string) {})
	m.atProfile = ATProfileML307C
	const (
		pdu     = "0004038177F7000862119121000000027532"
		initial = `+CPMS: "SM",1,10,"ME",1,180,"ME",1,180`
	)
	selectErr := errors.New("ME selection failed")
	done := make(chan []string, 1)
	go func() {
		selection := initial
		done <- respondToCommands(t, m, 8, func(req commandRequest) {
			switch req.cmd {
			case "AT+CPMS?":
				req.respChan <- selection
			case `AT+CPMS="ME","ME","ME"`:
				// The modem changed its read location despite returning an error.
				selection = `+CPMS: "ME",1,180,"ME",1,180,"ME",1,180`
				req.errChan <- selectErr
			case `AT+CPMS="SM","SM","SM"`:
				selection = `+CPMS: "SM",1,10,"SM",1,10,"SM",1,10`
				req.respChan <- "OK"
			case "AT+CMGL=4":
				if selection != `+CPMS: "SM",1,10,"SM",1,10,"SM",1,10` {
					req.errChan <- errors.New("listed the wrong store")
					return
				}
				req.respChan <- "+CMGL: 7,0,,17\n" + pdu
			case `AT+CPMS="SM","ME","ME"`:
				selection = initial
				req.respChan <- "OK"
			default:
				req.errChan <- errors.New("unexpected command " + req.cmd)
			}
		})
	}()

	err := m.WithSMSInbox(func(inbox *SMSInbox) error {
		err := inbox.Select("ME")
		if !errors.Is(err, selectErr) {
			t.Fatalf("ME selection error = %v", err)
		}
		if err := inbox.Select("SM"); err != nil {
			t.Fatal(err)
		}
		records, err := inbox.List()
		if err != nil || len(records) != 1 || records[0].PDU != pdu || !records[0].Received() {
			t.Fatalf("SM records = %+v, %v", records, err)
		}
		return selectErr
	})
	if !errors.Is(err, selectErr) {
		t.Fatalf("WithSMSInbox() error = %v", err)
	}
	want := []string{
		"AT+CPMS?", `AT+CPMS="ME","ME","ME"`, "AT+CPMS?", `AT+CPMS="SM","SM","SM"`,
		"AT+CPMS?", "AT+CMGL=4", `AT+CPMS="SM","ME","ME"`, "AT+CPMS?",
	}
	if got := <-done; !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
}
