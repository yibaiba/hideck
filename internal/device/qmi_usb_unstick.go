package device

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yibaiba/hideck/pkg/logger"
)

const (
	qmiCTLWedgeUnstickAfter = 2
	qmiUSBUnstickCooldown   = 10 * time.Minute
	qmiUSBUnstickHold       = 8 * time.Second
	qmiUSBUnstickOffHold    = 400 * time.Millisecond
	qmiUSBReauthorizeDelay  = 200 * time.Millisecond
	qmiUSBReauthorizeTries  = 3
)

var (
	usbDeviceAuthorizeFn     = writeUSBDeviceAuthorized
	usbModemAuthorizedPathFn = usbModemAuthorizedPath
	qmiControlPathExistsFn   = os.Stat
	qmiUSBUnstickSleepFn     = time.Sleep
	qmiUSBUnstickNowFn       = time.Now
	errUSBResetPathUnsafe    = errors.New("usb reset path is not a single modem device")
	errUSBResetAffectsOthers = errors.New("usb reset would affect another modem")
)

func qmiErrorIndicatesCTLWedged(message string) bool {
	if qmiErrorIndicatesTransportDown(message) {
		return false
	}
	message = strings.ToLower(strings.TrimSpace(message))
	if message == "" {
		return false
	}
	for _, fragment := range []string{
		"context deadline exceeded",
		"allocate client id",
		"ctl get_version_info",
		"transaction timed out",
		"服务未就绪",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func (w *Worker) markQMIUSBUnsticking(hold time.Duration) {
	if w == nil {
		return
	}
	if hold <= 0 {
		hold = qmiUSBUnstickHold
	}
	until := qmiUSBUnstickNowFn().Add(hold).UnixNano()
	w.qmiUSBUnstickUntil.Store(until)
}

func (w *Worker) markQMIUSBUnstickCompleted() {
	if w != nil {
		w.qmiLastUSBUnstick.Store(qmiUSBUnstickNowFn().UnixNano())
	}
}

func (w *Worker) isQMIUSBUnsticking() bool {
	if w == nil {
		return false
	}
	until := w.qmiUSBUnstickUntil.Load()
	return until > 0 && qmiUSBUnstickNowFn().UnixNano() < until
}

type qmiUSBUnstickRecord struct {
	at         time.Time
	resets     int
	skipLogged bool
}

// recordQMIUSBUnstick records a completed reset by USB path, so a Worker
// rebuilt for the same modem stays on cooldown.
func (p *Pool) recordQMIUSBUnstick(worker *Worker) {
	if p == nil || worker == nil {
		return
	}
	path := strings.TrimSpace(worker.Config.USBPath)
	if path == "" {
		return
	}
	now := qmiUSBUnstickNowFn()
	p.qmiUSBUnsticksMu.Lock()
	defer p.qmiUSBUnsticksMu.Unlock()
	if p.qmiUSBUnsticks == nil {
		p.qmiUSBUnsticks = make(map[string]*qmiUSBUnstickRecord)
	}
	record := p.qmiUSBUnsticks[path]
	if record == nil {
		record = &qmiUSBUnstickRecord{}
		p.qmiUSBUnsticks[path] = record
	}
	// Resets more than two cooldowns apart start a new run.
	if record.at.IsZero() || now.Sub(record.at) > 2*qmiUSBUnstickCooldown {
		record.resets = 0
	}
	record.at, record.resets, record.skipLogged = now, record.resets+1, false
}

// qmiUSBUnstickOnCooldown reports whether this modem was reset within the
// cooldown: by USB path, or by Worker when there is no path. The USB path
// record logs only the first skip of each cooldown window.
func (p *Pool) qmiUSBUnstickOnCooldown(worker *Worker) bool {
	if worker == nil {
		return false
	}
	path := strings.TrimSpace(worker.Config.USBPath)
	if p == nil || path == "" {
		return worker.qmiUSBUnstickOnCooldown()
	}
	now := qmiUSBUnstickNowFn()
	p.qmiUSBUnsticksMu.Lock()
	defer p.qmiUSBUnsticksMu.Unlock()
	record := p.qmiUSBUnsticks[path]
	if record == nil || record.at.IsZero() {
		return false
	}
	since := now.Sub(record.at)
	if since >= qmiUSBUnstickCooldown {
		return false
	}
	if !record.skipLogged {
		record.skipLogged = true
		logger.Warn("QMI CTL 仍卡死，USB 控制面复位在冷却中，暂不再复位",
			"device", worker.ID, "usb_path", path,
			"since_last_reset", since.Round(time.Second).String(), "cooldown", qmiUSBUnstickCooldown.String(),
			"resets_in_run", record.resets)
	}
	return true
}

func (w *Worker) qmiUSBUnstickOnCooldown() bool {
	if w == nil {
		return false
	}
	last := w.qmiLastUSBUnstick.Load()
	if last <= 0 {
		return false
	}
	return qmiUSBUnstickNowFn().Sub(time.Unix(0, last)) < qmiUSBUnstickCooldown
}

func writeUSBDeviceAuthorized(authorizedPath string, value string) error {
	return os.WriteFile(authorizedPath, []byte(value+"\n"), 0644)
}

func usbModemAuthorizedPath(usbPath string) (string, error) {
	usbPath = filepath.Clean(strings.TrimSpace(usbPath))
	const prefix = "/sys/bus/usb/devices/"
	if usbPath == "" || usbPath == "/" || !strings.HasPrefix(usbPath, prefix) {
		return "", errUSBResetPathUnsafe
	}
	rel := strings.TrimPrefix(usbPath, prefix)
	if rel == "" || strings.Contains(rel, "/") || strings.Contains(rel, ":") {
		return "", errUSBResetPathUnsafe
	}
	auth := filepath.Join(usbPath, "authorized")
	if _, err := os.Stat(auth); err != nil {
		return "", fmt.Errorf("usb authorized: %w", err)
	}
	return auth, nil
}

func usbResetWouldAffectOtherWorkers(usbPath string, others []string) bool {
	usbPath = filepath.Clean(strings.TrimSpace(usbPath))
	if usbPath == "" {
		return false
	}
	base := filepath.Base(usbPath)
	for _, other := range others {
		other = filepath.Clean(strings.TrimSpace(other))
		if other == "" || other == usbPath {
			continue
		}
		otherBase := filepath.Base(other)
		if strings.HasPrefix(otherBase, base+".") || strings.HasPrefix(other, usbPath+"/") {
			return true
		}
	}
	return false
}

func (p *Pool) otherWorkerUSBPaths(exceptID string) []string {
	if p == nil {
		return nil
	}
	var out []string
	for _, worker := range p.healthCheckWorkerSnapshot() {
		if worker == nil || worker.ID == exceptID {
			continue
		}
		if path := strings.TrimSpace(worker.Config.USBPath); path != "" {
			out = append(out, path)
		}
	}
	return out
}

func (p *Pool) maybeUnstickWedgedQMI(worker *Worker, startErr error, wedgeStreak *int) bool {
	if p == nil || worker == nil || startErr == nil || wedgeStreak == nil {
		return false
	}
	if worker.qmiUSBNeedsReauthorize.Load() {
		if err := p.reauthorizeWorkerUSBModem(worker); err != nil {
			logger.Warn("QMI USB 仍处于未授权状态，重新授权失败",
				"device", worker.ID, "err", err)
			return false
		}
		*wedgeStreak = 0
		return true
	}
	if !qmiErrorIndicatesCTLWedged(startErr.Error()) {
		*wedgeStreak = 0
		return false
	}
	*wedgeStreak++
	if *wedgeStreak < qmiCTLWedgeUnstickAfter {
		return false
	}
	if p.qmiUSBUnstickOnCooldown(worker) {
		return false
	}
	if p.volteCtl != nil {
		if p.volteCtl.ActiveCall(worker.ID) != nil {
			return false
		}
		if remain := p.volteCtl.VoiceUSBQuietRemaining(worker.ID); remain > 0 {
			return false
		}
	}
	if usbResetWouldAffectOtherWorkers(worker.Config.USBPath, p.otherWorkerUSBPaths(worker.ID)) {
		logger.Warn("QMI CTL 已卡死，但 USB 复位会影响到其他模组，跳过",
			"device", worker.ID, "usb_path", worker.Config.USBPath)
		return false
	}
	if !workerATModemAlive(worker) {
		logger.Debug("QMI CTL 超时但 AT 未就绪，跳过 USB 复位",
			"device", worker.ID)
		return false
	}
	if err := p.resetWorkerUSBModem(worker); err != nil {
		logger.Warn("QMI CTL 卡死，USB 复位失败",
			"device", worker.ID, "err", err)
		return false
	}
	*wedgeStreak = 0
	return true
}

func workerATModemAlive(worker *Worker) bool {
	if worker == nil {
		return false
	}
	atPort := worker.ResolvedATPort()
	if atPort == "" {
		return false
	}
	imei, err := probeIMEICachedFn(atPort, 1500*time.Millisecond)
	return err == nil && strings.TrimSpace(imei) != ""
}

func (p *Pool) resetWorkerUSBModem(worker *Worker) error {
	if worker == nil {
		return errors.New("worker_nil")
	}
	auth, err := usbModemAuthorizedPathFn(worker.Config.USBPath)
	if err != nil {
		return err
	}
	control := strings.TrimSpace(worker.Config.ControlDevice)
	logger.Warn("QMI CTL 已卡死且 AT 仍可用，复位本模组 USB 控制面（非 CFUN）",
		"device", worker.ID,
		"usb_path", worker.Config.USBPath,
		"control_device", control)
	if err := usbDeviceAuthorizeFn(auth, "0"); err != nil {
		return err
	}
	worker.qmiUSBNeedsReauthorize.Store(true)
	worker.markQMIUSBUnsticking(qmiUSBUnstickHold)
	qmiUSBUnstickSleepFn(qmiUSBUnstickOffHold)
	if err := p.reauthorizeWorkerUSBModemAt(worker, auth); err != nil {
		return err
	}
	if control == "" {
		return nil
	}
	deadline := qmiUSBUnstickNowFn().Add(qmiUSBUnstickHold)
	for qmiUSBUnstickNowFn().Before(deadline) {
		if _, err := qmiControlPathExistsFn(control); err == nil {
			return nil
		}
		qmiUSBUnstickSleepFn(200 * time.Millisecond)
	}
	return fmt.Errorf("usb reset: control device %s did not reappear", control)
}

func (p *Pool) reauthorizeWorkerUSBModem(worker *Worker) error {
	if worker == nil {
		return errors.New("worker_nil")
	}
	auth, err := usbModemAuthorizedPathFn(worker.Config.USBPath)
	if err != nil {
		return err
	}
	return p.reauthorizeWorkerUSBModemAt(worker, auth)
}

func (p *Pool) reauthorizeWorkerUSBModemAt(worker *Worker, auth string) error {
	if worker == nil {
		return errors.New("worker_nil")
	}
	worker.markQMIUSBUnsticking(qmiUSBUnstickHold)
	var lastErr error
	for attempt := 0; attempt < qmiUSBReauthorizeTries; attempt++ {
		if err := usbDeviceAuthorizeFn(auth, "1"); err == nil {
			worker.qmiUSBNeedsReauthorize.Store(false)
			worker.markQMIUSBUnstickCompleted()
			p.recordQMIUSBUnstick(worker)
			return nil
		} else {
			lastErr = err
		}
		if attempt+1 < qmiUSBReauthorizeTries {
			qmiUSBUnstickSleepFn(qmiUSBReauthorizeDelay)
		}
	}
	return fmt.Errorf("usb reset: reauthorize device: %w", lastErr)
}
