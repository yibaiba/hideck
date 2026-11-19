package modem

import (
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yibaiba/hideck/pkg/smscodec"
)

type smsSubmitWirePort struct {
	timeoutSerialPort
	input chan string
	done  chan struct{}
	write func([]byte) (int, error)
}

func (p *smsSubmitWirePort) Read(buf []byte) (int, error) {
	select {
	case data := <-p.input:
		return copy(buf, data), nil
	case <-p.done:
		return 0, io.EOF
	}
}

func (p *smsSubmitWirePort) Write(data []byte) (int, error) { return p.write(data) }

func (p *smsSubmitWirePort) Close() error {
	close(p.done)
	return nil
}

func TestML307CSMSSubmitWire(t *testing.T) {
	const payload = "000100038177F700000341E110\x1A"
	tests := []struct {
		name         string
		profile      ATProfile
		short        bool
		missingFinal bool
		wantErr      error
	}{
		{name: "ML307C success", profile: ATProfileML307C},
		{name: "ML307C result unknown", profile: ATProfileML307C, missingFinal: true, wantErr: ErrSMSSubmitUnknown},
		{name: "ML307C short payload write", profile: ATProfileML307C, short: true, wantErr: io.ErrShortWrite},
		{name: "legacy CRLF", profile: ATProfileDefault},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newRunningTestManager(t)
			m.atProfile = tt.profile
			wire := &smsSubmitWirePort{input: make(chan string, 2), done: make(chan struct{})}
			m.port = wire
			t.Cleanup(func() { m.StopAndWait(time.Second) })
			disconnected := make(chan string, 1)
			m.SetOnDisconnectWithReason(func(reason string) { disconnected <- reason })

			var writes []string
			wire.write = func(data []byte) (int, error) {
				n := len(data)
				switch len(writes) {
				case 0:
					wire.input <- "\r\n> "
				case 1:
					if tt.short {
						n--
					} else if !tt.missingFinal {
						if tt.profile == ATProfileML307C {
							// The final reply arrives after the prompt deadline.
							time.AfterFunc(250*time.Millisecond, func() { wire.input <- "\r\n+CMGS: 8\r\nOK\r\n" })
						} else {
							wire.input <- "\r\n+CMGS: 8\r\nOK\r\n"
						}
					}
				}
				writes = append(writes, string(data[:n]))
				return n, nil
			}

			req := commandRequest{
				cmd: "AT+CMGS=12", timeout: 200 * time.Millisecond,
				respChan: make(chan string, 1), errChan: make(chan error, 1),
				interactive: true, waitPrompt: true, followUp: payload,
			}
			if tt.profile == ATProfileML307C {
				req.smsFinalTimeout = time.Second
				if tt.missingFinal {
					req.smsFinalTimeout = 10 * time.Millisecond
				}
			}

			m.loopWG.Add(2)
			go func() {
				defer m.loopWG.Done()
				m.readLoop()
			}()
			go func() {
				defer m.loopWG.Done()
				m.handleCommand(<-m.cmdChanHigh)
			}()

			wantWire := []string{"AT+CMGS=12\r", payload}
			if tt.profile == ATProfileDefault {
				m.cmdChanHigh <- req
				select {
				case reply := <-req.respChan:
					if !strings.Contains(reply, "+CMGS: 8") {
						t.Fatalf("legacy reply = %q", reply)
					}
				case err := <-req.errChan:
					t.Fatal(err)
				case <-time.After(time.Second):
					t.Fatal("legacy submit did not complete")
				}
				wantWire[0] += "\n"
			} else if err := m.sendML307CSMSPart(req); !errors.Is(err, tt.wantErr) {
				t.Fatalf("submit error = %v, want %v", err, tt.wantErr)
			}
			if tt.short {
				wantWire[1] = payload[:len(payload)-1]
				wantWire = append(wantWire, "\x1B")
			}
			if tt.wantErr != nil {
				select {
				case reason := <-disconnected:
					if reason != "ml307c_sms_unsynchronized" {
						t.Fatalf("disconnect reason = %q", reason)
					}
				case <-time.After(time.Second):
					t.Fatal("failed submit did not retire the serial owner")
				}
				if _, err := m.ExecuteAT("AT+CIMI", time.Millisecond); err == nil {
					t.Fatal("AT query accepted on the retired serial owner")
				}
			}
			if !slices.Equal(writes, wantWire) {
				t.Fatalf("serial writes = %q, want %q", writes, wantWire)
			}
		})
	}
}

func TestML307CSMSSubmitPreservesConfirmedParts(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "unknown submit", err: ErrSMSSubmitUnknown},
		{name: "explicit rejection", err: errors.New("modem rejected submit")},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := newRunningTestManager(t)
			m.atProfile = ATProfileML307C
			commands := make(chan []string, 1)
			go func() {
				var seen []string
				for i := 0; i < 3; i++ {
					req := <-m.cmdChanHigh
					seen = append(seen, req.cmd)
					if i == 2 {
						req.errChan <- test.err
					} else {
						req.respChan <- "OK"
					}
				}
				commands <- seen
			}()
			err := m.SendSMSWithOptions("777", strings.Repeat("A", 320), smscodec.SubmitOptions{})
			var progress *SMSSubmitError
			if !errors.Is(err, test.err) || !errors.As(err, &progress) || progress.PartsConfirmed != 1 || progress.PartsTotal != 3 {
				t.Fatalf("send progress = %+v, err = %v", progress, err)
			}
			seen := <-commands
			if seen[0] != "AT+CMGF=0" || !strings.HasPrefix(seen[1], "AT+CMGS=") || !strings.HasPrefix(seen[2], "AT+CMGS=") {
				t.Fatalf("commands = %v", seen)
			}
			select {
			case req := <-m.cmdChanHigh:
				t.Fatalf("unexpected submission after a segment failed: %q", req.cmd)
			default:
			}
		})
	}
}
