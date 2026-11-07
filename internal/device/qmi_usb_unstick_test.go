package device

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/pkg/logger"
)

func TestQMIErrorIndicatesCTLWedged(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"allocate client ID request failed after retries: context deadline exceeded", true},
		{"CTL GET_VERSION_INFO 请求失败: context deadline exceeded", true},
		{"QMI 服务未就绪: DMS", true},
		{"write failed: write unix @->@qmi-proxy: write: broken pipe", false},
		{"connection closed", false},
		{"refresh_identity: live_identity_empty", false},
	}
	for _, tc := range cases {
		if got := qmiErrorIndicatesCTLWedged(tc.msg); got != tc.want {
			t.Fatalf("qmiErrorIndicatesCTLWedged(%q)=%v want %v", tc.msg, got, tc.want)
		}
	}
}

func TestUSBModemAuthorizedPathRejectsHubAndInterface(t *testing.T) {
	if _, err := usbModemAuthorizedPath("/sys/bus/usb/devices/1-2/1-2:1.4"); err == nil {
		t.Fatal("interface path must be rejected")
	}
	if _, err := usbModemAuthorizedPath("/tmp/1-2.1"); err == nil {
		t.Fatal("non-sysfs path must be rejected")
	}
}

func TestUSBResetWouldAffectOtherWorkers(t *testing.T) {
	if !usbResetWouldAffectOtherWorkers("/sys/bus/usb/devices/1-2", []string{"/sys/bus/usb/devices/1-2.1"}) {
		t.Fatal("resetting hub 1-2 must be treated as affecting 1-2.1")
	}
	if usbResetWouldAffectOtherWorkers("/sys/bus/usb/devices/1-2.1", []string{"/sys/bus/usb/devices/1-2.2"}) {
		t.Fatal("resetting 1-2.1 must not affect sibling 1-2.2")
	}
}

func TestMaybeUnstickWedgedQMIResetsOnlyAfterStreakAndATAlive(t *testing.T) {
	origAuth := usbModemAuthorizedPathFn
	origWrite := usbDeviceAuthorizeFn
	origExists := qmiControlPathExistsFn
	origSleep := qmiUSBUnstickSleepFn
	origIMEI := probeIMEICachedFn
	t.Cleanup(func() {
		usbModemAuthorizedPathFn = origAuth
		usbDeviceAuthorizeFn = origWrite
		qmiControlPathExistsFn = origExists
		qmiUSBUnstickSleepFn = origSleep
		probeIMEICachedFn = origIMEI
	})
	qmiUSBUnstickSleepFn = func(time.Duration) {}
	probeIMEICachedFn = func(atPort string, timeout time.Duration) (string, error) {
		if atPort != "/dev/ttyUSB2" {
			t.Fatalf("atPort=%q", atPort)
		}
		return "860000000000001", nil
	}
	writes := []string{}
	usbModemAuthorizedPathFn = func(usbPath string) (string, error) {
		if !strings.HasSuffix(usbPath, "1-2.1") {
			t.Fatalf("usbPath=%q", usbPath)
		}
		return "/tmp/authorized-test", nil
	}
	usbDeviceAuthorizeFn = func(path, value string) error {
		writes = append(writes, value)
		return nil
	}
	qmiControlPathExistsFn = func(name string) (os.FileInfo, error) {
		return nil, nil
	}

	p := NewPool(&config.Config{})
	defer p.cancel()
	worker := &Worker{
		ID: "wwan1",
		Config: config.DeviceConfig{
			ID:            "wwan1",
			ATPort:        "/dev/ttyUSB2",
			USBPath:       "/sys/bus/usb/devices/1-2.1",
			ControlDevice: "/dev/cdc-wdm0",
		},
	}
	streak := 0
	if p.maybeUnstickWedgedQMI(worker, errors.New("context deadline exceeded"), &streak) {
		t.Fatal("first CTL timeout must not USB-reset")
	}
	if streak != 1 {
		t.Fatalf("streak=%d want 1", streak)
	}
	if p.maybeUnstickWedgedQMI(worker, errors.New("allocate client ID request failed after retries: context deadline exceeded"), &streak) != true {
		t.Fatal("second CTL timeout with AT alive should USB-reset")
	}
	if len(writes) != 2 || writes[0] != "0" || writes[1] != "1" {
		t.Fatalf("writes=%v want [0 1]", writes)
	}
	if !worker.isQMIUSBUnsticking() {
		t.Fatal("worker should suppress health during USB unstick")
	}
}

func TestMaybeUnstickWedgedQMISkipsBrokenPipe(t *testing.T) {
	p := NewPool(&config.Config{})
	defer p.cancel()
	w := &Worker{ID: "wwan1"}
	streak := 2
	if p.maybeUnstickWedgedQMI(w, errors.New("write unix @->@qmi-proxy: write: broken pipe"), &streak) {
		t.Fatal("broken pipe is transport-down, not a USB unstick")
	}
	if streak != 0 {
		t.Fatalf("streak=%d want 0 after non-wedge error", streak)
	}
}

func TestResetWorkerUSBModemRetriesReauthorization(t *testing.T) {
	origAuth := usbModemAuthorizedPathFn
	origWrite := usbDeviceAuthorizeFn
	origExists := qmiControlPathExistsFn
	origSleep := qmiUSBUnstickSleepFn
	t.Cleanup(func() {
		usbModemAuthorizedPathFn = origAuth
		usbDeviceAuthorizeFn = origWrite
		qmiControlPathExistsFn = origExists
		qmiUSBUnstickSleepFn = origSleep
	})
	usbModemAuthorizedPathFn = func(string) (string, error) { return "/tmp/authorized-test", nil }
	qmiControlPathExistsFn = func(string) (os.FileInfo, error) { return nil, nil }
	qmiUSBUnstickSleepFn = func(time.Duration) {}
	writes := make([]string, 0, 3)
	usbDeviceAuthorizeFn = func(_ string, value string) error {
		writes = append(writes, value)
		if value == "1" && len(writes) == 2 {
			return errors.New("temporary sysfs failure")
		}
		return nil
	}

	worker := &Worker{Config: config.DeviceConfig{USBPath: "/sys/bus/usb/devices/1-2.1", ControlDevice: "/dev/cdc-wdm0"}}
	if err := (&Pool{}).resetWorkerUSBModem(worker); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(writes, ","); got != "0,1,1" {
		t.Fatalf("authorization writes=%s want 0,1,1", got)
	}
	if worker.qmiUSBNeedsReauthorize.Load() || !worker.qmiUSBUnstickOnCooldown() {
		t.Fatal("successful reauthorization must clear pending state and start cooldown")
	}
}

func TestFailedUSBReauthorizationRemainsRepairable(t *testing.T) {
	origAuth := usbModemAuthorizedPathFn
	origWrite := usbDeviceAuthorizeFn
	origSleep := qmiUSBUnstickSleepFn
	t.Cleanup(func() {
		usbModemAuthorizedPathFn = origAuth
		usbDeviceAuthorizeFn = origWrite
		qmiUSBUnstickSleepFn = origSleep
	})
	usbModemAuthorizedPathFn = func(string) (string, error) { return "/tmp/authorized-test", nil }
	qmiUSBUnstickSleepFn = func(time.Duration) {}
	allow := false
	usbDeviceAuthorizeFn = func(_ string, value string) error {
		if value == "1" && !allow {
			return errors.New("sysfs unavailable")
		}
		return nil
	}

	worker := &Worker{Config: config.DeviceConfig{USBPath: "/sys/bus/usb/devices/1-2.1"}}
	if err := (&Pool{}).resetWorkerUSBModem(worker); err == nil {
		t.Fatal("expected reauthorization failure")
	}
	if !worker.qmiUSBNeedsReauthorize.Load() || worker.qmiUSBUnstickOnCooldown() {
		t.Fatal("failed reauthorization must remain pending without cooldown")
	}
	allow = true
	streak := 2
	if !(&Pool{}).maybeUnstickWedgedQMI(worker, errors.New("broken pipe"), &streak) {
		t.Fatal("pending reauthorization was not repaired before AT/QMI checks")
	}
	if worker.qmiUSBNeedsReauthorize.Load() || !worker.qmiUSBUnstickOnCooldown() {
		t.Fatal("repaired authorization did not complete recovery state")
	}
}

func TestQMITransportDownDoesNotOverrideUSBUnstick(t *testing.T) {
	if qmiTransportDownOverridesSuppression(true, "qmi_usb_unstick") {
		t.Fatal("USB unstick must not be overridden by broken pipe; the reset itself drops the fd")
	}
}

// hideck#94: every reset drops the modem and the Worker is rebuilt, so the
// cooldown must follow the USB path, not the Worker.
func TestQMIUSBUnstickCooldownSurvivesWorkerRebuild(t *testing.T) {
	origAuth := usbModemAuthorizedPathFn
	origWrite := usbDeviceAuthorizeFn
	origExists := qmiControlPathExistsFn
	origSleep := qmiUSBUnstickSleepFn
	origNow := qmiUSBUnstickNowFn
	origProbe := probeIMEICachedFn
	t.Cleanup(func() {
		usbModemAuthorizedPathFn = origAuth
		usbDeviceAuthorizeFn = origWrite
		qmiControlPathExistsFn = origExists
		qmiUSBUnstickSleepFn = origSleep
		qmiUSBUnstickNowFn = origNow
		probeIMEICachedFn = origProbe
	})
	now := time.Unix(1_800_000_000, 0)
	qmiUSBUnstickNowFn = func() time.Time { return now }
	usbModemAuthorizedPathFn = func(string) (string, error) { return "/tmp/authorized-test", nil }
	qmiControlPathExistsFn = func(string) (os.FileInfo, error) { return nil, nil }
	qmiUSBUnstickSleepFn = func(time.Duration) {}
	probeIMEICachedFn = func(string, time.Duration) (string, error) { return "490154203237518", nil }
	resets := 0
	usbDeviceAuthorizeFn = func(_ string, value string) error {
		if value == "0" {
			resets++
		}
		return nil
	}
	wedged := errors.New("allocate client ID request failed after retries: context deadline exceeded")
	newWorker := func(path string) *Worker {
		return &Worker{ID: "wwan0", Config: config.DeviceConfig{
			USBPath: path, ControlDevice: "/dev/cdc-wdm0", ATPort: "/dev/ttyUSB2",
		}}
	}
	pool := &Pool{}

	streak := qmiCTLWedgeUnstickAfter
	if !pool.maybeUnstickWedgedQMI(newWorker("/sys/bus/usb/devices/1-7"), wedged, &streak) || resets != 1 {
		t.Fatalf("first wedge did not reset the modem: resets=%d", resets)
	}
	logger.Setup(logger.LogConfig{Debug: true, Filename: filepath.Join(t.TempDir(), "app.log")})
	logs := logger.GlobalBroadcaster.Subscribe()
	defer logger.GlobalBroadcaster.Unsubscribe(logs)
	for _, after := range []time.Duration{38 * time.Second, 38 * time.Second} {
		now = now.Add(after)
		streak = qmiCTLWedgeUnstickAfter
		if pool.maybeUnstickWedgedQMI(newWorker("/sys/bus/usb/devices/1-7"), wedged, &streak) || resets != 1 {
			t.Fatalf("rebuilt Worker reset the same modem again within the cooldown: resets=%d", resets)
		}
	}
	var skips []logger.LogEntry
	for drained := false; !drained; {
		select {
		case entry := <-logs:
			if strings.Contains(entry.Message, "USB 控制面复位在冷却中") {
				skips = append(skips, entry)
			}
		default:
			drained = true
		}
	}
	if len(skips) != 1 {
		t.Fatalf("cooldown skip logged %d times, want once per window", len(skips))
	}
	if fields := readLogFields(t, skips[0]); fields["usb_path"] != "/sys/bus/usb/devices/1-7" ||
		fields["since_last_reset"] != "38s" || fields["cooldown"] != "10m0s" || fields["resets_in_run"] != float64(1) {
		t.Fatalf("cooldown skip fields = %v", fields)
	}
	streak = qmiCTLWedgeUnstickAfter
	if !pool.maybeUnstickWedgedQMI(newWorker("/sys/bus/usb/devices/1-8"), wedged, &streak) || resets != 2 {
		t.Fatalf("another modem was held by this modem's cooldown: resets=%d", resets)
	}
	now = now.Add(qmiUSBUnstickCooldown)
	streak = qmiCTLWedgeUnstickAfter
	if !pool.maybeUnstickWedgedQMI(newWorker("/sys/bus/usb/devices/1-7"), wedged, &streak) || resets != 3 {
		t.Fatalf("cooldown never expired: resets=%d", resets)
	}
}
