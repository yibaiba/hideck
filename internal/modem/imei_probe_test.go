package modem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yibaiba/hideck/internal/config"
	"go.bug.st/serial"
)

type imeiProbeSerialPort struct {
	serial.Port
	reply   *strings.Reader
	written []string
}

func (p *imeiProbeSerialPort) SetReadTimeout(time.Duration) error { return nil }
func (p *imeiProbeSerialPort) Close() error                       { return nil }
func (p *imeiProbeSerialPort) Read(b []byte) (int, error)         { return p.reply.Read(b) }
func (p *imeiProbeSerialPort) Write(b []byte) (int, error) {
	p.written = append(p.written, string(b))
	return len(b), nil
}

func TestLiveIMEIProbeBypassesCacheForUnchangedTTYNode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ttyUSB8")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	node, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	const cachedIMEI, replacementIMEI = "490154203237518", "490154203237526"
	t.Cleanup(func() {
		imeiCache.mu.Lock()
		delete(imeiCache.m, path)
		imeiCache.mu.Unlock()
	})
	storeCachedIMEI(path, cachedIMEI, node)

	serialPort := &imeiProbeSerialPort{reply: strings.NewReader("\r\n+CGSN: " + replacementIMEI + "\r\n\r\nOK\r\n")}
	origOpen := openIMEISerialPortFn
	t.Cleanup(func() { openIMEISerialPortFn = origOpen })
	openIMEISerialPortFn = func(_ context.Context, port string, _ *serial.Mode) (serial.Port, error) {
		if port != path {
			t.Fatalf("probe opened %q, want %q", port, path)
		}
		return serialPort, nil
	}
	live, err := ATProfileML307C.ProbeIMEI(context.Background(), path, time.Second)
	if err != nil || live != replacementIMEI {
		t.Fatalf("live probe = %q, %v", live, err)
	}
	if want := []string{"AT\r\n", "AT+CGSN=1\r\n"}; !reflect.DeepEqual(serialPort.written, want) {
		t.Fatalf("AT commands = %q, want %q", serialPort.written, want)
	}

	cached, err := ProbeIMEICached(path, time.Second)
	if err != nil || cached != cachedIMEI || len(serialPort.written) != 2 {
		t.Fatalf("default cached probe = %q, %v, commands %q", cached, err, serialPort.written)
	}
}

type openedIMEISerialPort struct {
	failingSerialPort
	probeCommand string
	replies      [][]string
	response     []string
	commands     atomic.Int32
	probes       atomic.Int32
	terminalRead atomic.Bool
	inputFlushed bool
	earlyInit    atomic.Bool
}

func (p *openedIMEISerialPort) ResetInputBuffer() error {
	p.inputFlushed = true
	return nil
}

func (p *openedIMEISerialPort) Write(b []byte) (int, error) {
	p.commands.Add(1)
	if p.inputFlushed {
		p.inputFlushed = false
		if string(b) != p.probeCommand || len(p.replies) == 0 {
			return 0, errors.New("unexpected IMEI probe")
		}
		p.probes.Add(1)
		p.terminalRead.Store(false)
		p.response = p.replies[0]
		p.replies = p.replies[1:]
		return len(b), nil
	}
	if !p.terminalRead.Load() {
		p.earlyInit.Store(true)
	}
	return 0, errors.New("fake device stops after IMEI")
}

func (p *openedIMEISerialPort) Read(b []byte) (int, error) {
	if len(p.response) == 0 {
		time.Sleep(time.Millisecond)
		return 0, nil
	}
	part := p.response[0]
	p.response = p.response[1:]
	if part == "\n" {
		p.terminalRead.Store(true)
	}
	return copy(b, part), nil
}

func TestManagerVerifiesTheOpenedPortBeforeATInitialization(t *testing.T) {
	const expected = "490154203237518"
	original := openManagerSerialPort
	t.Cleanup(func() { openManagerSerialPort = original })
	match := []string{"+CGSN: " + expected + "\r\n", "OK\r", "\n"}
	empty := []string{"OK\r", "\n"}
	for _, tc := range []struct {
		name, wantError string
		profile         ATProfile
		replies         [][]string
	}{
		{name: "matched", profile: ATProfileML307C, replies: [][]string{match}},
		{name: "replacement", profile: ATProfileML307C, replies: [][]string{{"+CGSN: 490154203237526\r\n", "OK\r", "\n"}}, wantError: "实时 IMEI 与配置不匹配"},
		{name: "unreadable after retry", profile: ATProfileML307C, replies: [][]string{empty, empty}, wantError: "无法验证"},
		{name: "default AT retries ERROR", profile: ATProfileDefault, replies: [][]string{{"ERROR\r", "\n"}, {expected + "\r\n", "OK\r", "\n"}}},
		{name: "timeout then matched", profile: ATProfileML307C, replies: [][]string{nil, match}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := NewWithATProfile(config.DeviceConfig{
				ID: "imei-gate", DeviceBackend: "at", ATPort: filepath.Join(t.TempDir(), "ttyUSB8"),
			}, tc.profile)
			if err != nil {
				t.Fatal(err)
			}
			port := &openedIMEISerialPort{
				probeCommand: tc.profile.dialect().imei.command + "\r\n",
				replies:      tc.replies,
			}
			opens := 0
			openManagerSerialPort = func(string, *serial.Mode) (serial.Port, error) {
				opens++
				if opens == 1 {
					return nil, errors.New("device or resource busy")
				}
				return port, nil
			}
			err = m.StartWithExpectedIMEI(expected)
			wantProbes := int32(len(tc.replies))
			if opens != 2 || port.probes.Load() != wantProbes || !port.terminalRead.Load() || port.earlyInit.Load() {
				m.Stop()
				t.Fatalf("opens=%d probes=%d want=%d terminalRead=%v earlyInit=%v",
					opens, port.probes.Load(), wantProbes, port.terminalRead.Load(), port.earlyInit.Load())
			}
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) || !port.closed.Load() ||
					m.CanExecuteAT() || port.commands.Load() != wantProbes {
					t.Fatalf("err=%v closed=%v running=%v commands=%d", err, port.closed.Load(), m.CanExecuteAT(), port.commands.Load())
				}
				return
			}
			defer m.Stop()
			if err != nil || !m.CanExecuteAT() || port.closed.Load() {
				t.Fatalf("err=%v running=%v closed=%v", err, m.CanExecuteAT(), port.closed.Load())
			}
		})
	}
}
