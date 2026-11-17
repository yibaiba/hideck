package device

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/yibaiba/hideck/internal/backend"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/pkg/logger"
)

var qmiControlStatFn = os.Stat
var qmiRecoveryControlStableInterval = 1200 * time.Millisecond

func workerATProbeOK(w *Worker, timeout time.Duration) bool {
	if w != nil {
		if resolvedBackendMode(w.Config) == backend.BackendQMI {
			return true
		}
		if w.Backend != nil && w.Backend.Mode() == backend.BackendQMI {
			return true
		}
	}
	if w == nil || w.Modem == nil || !w.Modem.HasATPort() {
		return true
	}
	if !w.Modem.CanExecuteAT() {
		return false
	}
	_, err := w.Modem.ExecuteATSilent("AT", timeout)
	return err == nil
}

func (p *Pool) refreshModemRebootRecoveredIdentity(w *Worker, reason string) error {
	if w == nil {
		return fmt.Errorf("worker_nil")
	}
	if reason = strings.TrimSpace(reason); reason == "" {
		reason = "modem_reboot_recovery"
	}

	result, err := p.refreshIdentityAndApplyCardPolicy(w, reason)
	if err != nil {
		return fmt.Errorf("refresh_identity: %w", err)
	}

	if strings.TrimSpace(result.ICCID) == "" && strings.TrimSpace(result.IMSI) == "" {
		return fmt.Errorf("sim_identity_empty")
	}

	return nil
}

func (p *Pool) markQMIControlRecovered(worker *Worker, reason string) {
	if p == nil || worker == nil {
		return
	}
	if reason = strings.TrimSpace(reason); reason == "" {
		reason = "qmi_control_recovered"
	}
	worker.RecordWatchdogEvent(WatchdogEvent{
		Layer:     HealthLayerQMI,
		State:     HealthStateHealthy,
		EventType: "qmi_control_recovered",
		Reason:    reason,
		At:        time.Now(),
	})
	worker.resetHealthFailureStreak()
	if p.lifecycle != nil {
		p.lifecycle.FinishOnline(worker.ID)
	}
}

type modemRebootRecoveryOptions struct {
	initialWorker          *Worker // Optional ownership guard for an asynchronous reset.
	deviceID               string
	reason                 string
	delays                 []time.Duration
	removeBeforeScan       bool
	restoreVoWiFi          bool
	transportEvent         *TransportRecoveryEvent
	transportEventObserved bool
}

func defaultModemRebootRecoveryOptions(deviceID string, reason string) modemRebootRecoveryOptions {
	return modemRebootRecoveryOptions{
		deviceID:         deviceID,
		reason:           reason,
		delays:           []time.Duration{0, time.Second, 3 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second},
		removeBeforeScan: true,
		restoreVoWiFi:    true,
	}
}

// manualRebootRecoveryDelays 为手动重启专用扫描节奏：去掉 delay=0 的立即轮，
// 首轮等待模组真正复位后再扫描，避免命中尚未掉线的旧模组。
func manualRebootRecoveryDelays() []time.Duration {
	return []time.Duration{2 * time.Second, 3 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second}
}

// usbUnplugRecoveryDelays 给 USB 拔出留更长窗口：udev 会提前唤醒，
// 超时轮则兜住 udev 漏事件或用户稍后插回的情况。
func usbUnplugRecoveryDelays() []time.Duration {
	return []time.Duration{
		0,
		time.Second,
		3 * time.Second,
		5 * time.Second,
		10 * time.Second,
		20 * time.Second,
		30 * time.Second,
		time.Minute,
		2 * time.Minute,
	}
}

func recoveryDelaysForReason(reason string) []time.Duration {
	switch strings.TrimSpace(reason) {
	case "manual_reboot":
		return manualRebootRecoveryDelays()
	case "qmi_transport_down", "qmi_transport_failed", "qmi_health_threshold":
		return usbUnplugRecoveryDelays()
	default:
		return defaultModemRebootRecoveryOptions("", "").delays
	}
}

func closeIfOpen(ch chan struct{}) {
	if ch == nil {
		return
	}
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func (p *Pool) beginModemRebootRecovery(deviceID string) bool {
	_, ok := p.startModemRebootRecovery(deviceID)
	return ok
}

func (p *Pool) startModemRebootRecovery(deviceID string) (uint64, bool) {
	if p == nil || deviceID == "" {
		return 0, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.modemRebootRecovering == nil {
		p.modemRebootRecovering = make(map[string]bool)
	}
	if p.modemRebootRecovering[deviceID] {
		return 0, false
	}
	if p.modemRebootGeneration == nil {
		p.modemRebootGeneration = make(map[string]uint64)
	}
	if p.modemRebootWakeups == nil {
		p.modemRebootWakeups = make(map[string]chan struct{})
	}
	if p.modemRebootCancels == nil {
		p.modemRebootCancels = make(map[string]chan struct{})
	}
	p.modemRebootGeneration[deviceID]++
	generation := p.modemRebootGeneration[deviceID]
	p.modemRebootRecovering[deviceID] = true
	p.modemRebootWakeups[deviceID] = make(chan struct{}, 1)
	p.modemRebootCancels[deviceID] = make(chan struct{})
	return generation, true
}

func (p *Pool) finishModemRebootRecovery(deviceID string) {
	if p == nil || deviceID == "" {
		return
	}
	p.mu.RLock()
	generation := p.modemRebootGeneration[deviceID]
	p.mu.RUnlock()
	p.finishModemRebootRecoveryGen(deviceID, generation)
}

func (p *Pool) finishModemRebootRecoveryGen(deviceID string, generation uint64) bool {
	if p == nil || deviceID == "" || generation == 0 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.modemRebootGeneration[deviceID] != generation {
		return false
	}
	closeIfOpen(p.modemRebootCancels[deviceID])
	delete(p.modemRebootRecovering, deviceID)
	delete(p.modemRebootWakeups, deviceID)
	delete(p.modemRebootCancels, deviceID)
	return true
}

func (p *Pool) abandonModemRebootRecovery(deviceID string) {
	if p == nil || deviceID == "" {
		return
	}
	p.mu.Lock()
	if p.modemRebootGeneration == nil {
		p.modemRebootGeneration = make(map[string]uint64)
	}
	p.modemRebootGeneration[deviceID]++
	closeIfOpen(p.modemRebootCancels[deviceID])
	delete(p.modemRebootRecovering, deviceID)
	delete(p.modemRebootWakeups, deviceID)
	delete(p.modemRebootCancels, deviceID)
	p.mu.Unlock()
	if p.transportRecovery != nil {
		p.transportRecovery.Finish(deviceID)
	}
}

func (p *Pool) modemRebootRecoveryStillCurrent(deviceID string, generation uint64) bool {
	if p == nil || deviceID == "" || generation == 0 {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.modemRebootRecovering[deviceID] && p.modemRebootGeneration[deviceID] == generation
}

func (p *Pool) modemRebootWakeChannel(deviceID string) <-chan struct{} {
	if p == nil || deviceID == "" {
		return nil
	}
	p.mu.RLock()
	ch := p.modemRebootWakeups[deviceID]
	p.mu.RUnlock()
	return ch
}

func (p *Pool) modemRebootCancelChannel(deviceID string) <-chan struct{} {
	if p == nil || deviceID == "" {
		return nil
	}
	p.mu.RLock()
	ch := p.modemRebootCancels[deviceID]
	p.mu.RUnlock()
	return ch
}

func (p *Pool) waitModemRebootRecoveryTrigger(deviceID string, delay time.Duration) {
	ch := p.modemRebootWakeChannel(deviceID)
	cancel := p.modemRebootCancelChannel(deviceID)
	if delay <= 0 {
		select {
		case <-ch:
		case <-cancel:
		default:
		}
		return
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ch:
	case <-cancel:
	case <-p.ctx.Done():
	}
}

func (p *Pool) WakeModemRebootRecoveries(reason string) int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	woken := 0
	for deviceID, ch := range p.modemRebootWakeups {
		if !p.modemRebootRecovering[deviceID] || ch == nil {
			continue
		}
		select {
		case ch <- struct{}{}:
			woken++
		default:
		}
	}
	return woken
}

func modemRebootRecoveryConfig(deviceID string) (config.DeviceConfig, bool) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return config.DeviceConfig{}, false
	}
	if cfg, err := config.GetDeviceByID(deviceID); err == nil && cfg != nil {
		return *cfg, true
	}
	return config.DeviceConfig{}, false
}

func qmiControlPathStatOK(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := qmiControlStatFn(path)
	return err == nil
}

func qmiRecoveryControlPathStable(cfg config.DeviceConfig, interval time.Duration) bool {
	if !requiresQMICore(cfg) {
		return true
	}
	controlPath := strings.TrimSpace(cfg.ControlDevice)
	if controlPath == "" {
		return true
	}
	if !qmiControlPathStatOK(controlPath) {
		return false
	}
	if interval > 0 {
		time.Sleep(interval)
	}
	return qmiControlPathStatOK(controlPath)
}

type qmiRecoveryLiveCandidate struct {
	Device QMIDevice
	IMEI   string
}

type qmiRecoveryScanDecision struct {
	Ready      bool
	Reason     string
	Attachment QMIDevice
}

func qmiRecoveryScanGate(cfg config.DeviceConfig, live []qmiRecoveryLiveCandidate, discoveryAvailable bool) qmiRecoveryScanDecision {
	if !requiresQMICore(cfg) {
		return qmiRecoveryScanDecision{Ready: true, Reason: "non_qmi"}
	}
	configuredIMEI := strings.TrimSpace(cfg.ModemIMEI)
	if configuredIMEI != "" {
		for _, candidate := range live {
			if config.IMEIMatches(candidate.IMEI, configuredIMEI) {
				return qmiRecoveryScanDecision{
					Ready:      true,
					Reason:     "live_imei_match",
					Attachment: candidate.Device,
				}
			}
		}
	}
	if !discoveryAvailable {
		if qmiRecoveryControlPathStable(cfg, qmiRecoveryControlStableInterval) {
			return qmiRecoveryScanDecision{Ready: true, Reason: "configured_control_stable"}
		}
		return qmiRecoveryScanDecision{Ready: false, Reason: "configured_control_missing"}
	}
	for _, candidate := range live {
		if strings.TrimSpace(candidate.Device.ControlPath) == strings.TrimSpace(cfg.ControlDevice) ||
			strings.TrimSpace(candidate.Device.NetInterface) == strings.TrimSpace(cfg.Interface) ||
			strings.TrimSpace(candidate.Device.USBPath) == strings.TrimSpace(cfg.USBPath) {
			return qmiRecoveryScanDecision{
				Ready:      true,
				Reason:     "configured_attachment_seen",
				Attachment: candidate.Device,
			}
		}
	}
	return qmiRecoveryScanDecision{Ready: false, Reason: "no_matching_qmi_attachment"}
}

func (p *Pool) qmiRecoveryLiveCandidates(cfg config.DeviceConfig) ([]qmiRecoveryLiveCandidate, bool) {
	discovered, err := discoverQMIDevicesFn()
	if err != nil {
		logger.Warn("模组重启恢复：QMI attachment 扫描失败，将回退到配置控制口检查",
			"device", strings.TrimSpace(cfg.ID),
			"err", err)
		return nil, false
	}
	candidates := make([]qmiRecoveryLiveCandidate, 0, len(discovered))
	liveWorkerIndex := BuildWorkerDiscoveryIndex(p.GetAllWorkers(), false)
	for _, raw := range discovered {
		if liveInfo, ok := liveWorkerIndex.Lookup(raw.ControlPath, raw.USBPath, raw.NetInterface); ok && containsPort(raw.ATPorts, liveInfo.ATPort) {
			raw.ATPort = liveInfo.ATPort
		}
		dev, imei := enrichDiscoveredQMIDeviceATFirst(raw, 3*time.Second, true)
		logger.Debug("模组重启恢复：发现 QMI 候选",
			"device", strings.TrimSpace(cfg.ID),
			"imei", imei,
			"control", dev.ControlPath,
			"interface", dev.NetInterface,
			"usb_path", dev.USBPath)
		candidates = append(candidates, qmiRecoveryLiveCandidate{
			Device: dev,
			IMEI:   imei,
		})
	}
	return candidates, true
}

func qmiStartCoreFailureShouldAbortWorker(message string) bool {
	return qmiErrorIndicatesTransportDown(message)
}

func modemRebootRecoveryShouldRebuildAfterReadinessFailure(opts modemRebootRecoveryOptions, err error) bool {
	if err == nil {
		return false
	}
	reason := strings.TrimSpace(opts.reason)
	if reason != "manual_reboot" && !opts.removeBeforeScan {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, fragment := range []string{
		"live_identity_empty",
		"sim_identity_empty",
		"refresh_identity:",
		"refresh_runtime:",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func qmiWorkerControlReady(worker *Worker) bool {
	if worker == nil {
		return false
	}
	snapshot := worker.HealthSnapshot()
	return snapshot.State == HealthStateHealthy && snapshot.Layer == HealthLayerQMI
}

func modemRebootRecoveryShouldRebuildAfterReadinessFailureForWorker(opts modemRebootRecoveryOptions, worker *Worker, err error) bool {
	if err == nil {
		return false
	}
	if worker != nil && worker.isQMICoreStarting() {
		return false
	}
	if qmiWorkerControlReady(worker) {
		message := strings.ToLower(err.Error())
		for _, fragment := range []string{
			"refresh_identity:",
			"sim_identity_empty",
			"identity not readable",
			"live_identity_empty",
		} {
			if strings.Contains(message, fragment) {
				return false
			}
		}
	}
	return modemRebootRecoveryShouldRebuildAfterReadinessFailure(opts, err)
}

// modemRebootRecoveryShouldRebuildAfterTransportDown 判断身份刷新失败是否源于
// 控制面传输已真正断开。仅当 Worker 当前控制面不健康且错误为传输断开时返回 true。
func modemRebootRecoveryShouldRebuildAfterTransportDown(worker *Worker, err error) bool {
	if err == nil {
		return false
	}
	if qmiWorkerControlReady(worker) {
		return false
	}
	return qmiErrorIndicatesTransportDown(err.Error())
}

func (p *Pool) ScheduleModemRebootRecovery(deviceID string, reason string) {
	opts := defaultModemRebootRecoveryOptions(deviceID, reason)
	opts.delays = recoveryDelaysForReason(reason)
	go p.runModemRebootRecovery(opts)
}

// ScheduleModemRebootRecoveryForWorker binds a manually requested reset to the
// Worker that received it. A replacement with the same ID must not inherit the
// old modem's recovery loop.
func (p *Pool) ScheduleModemRebootRecoveryForWorker(worker *Worker, reason string) bool {
	if p == nil || worker == nil || p.GetWorker(worker.ID) != worker {
		return false
	}
	opts := defaultModemRebootRecoveryOptions(worker.ID, reason)
	opts.delays = recoveryDelaysForReason(reason)
	opts.initialWorker = worker
	go p.runModemRebootRecovery(opts)
	return true
}

func (p *Pool) scheduleWorkerRecoveryWithTransportEvent(deviceID string, reason string, event *TransportRecoveryEvent) bool {
	deviceID = strings.TrimSpace(deviceID)
	reason = strings.TrimSpace(reason)
	if p == nil || deviceID == "" {
		return false
	}
	if reason == "" {
		reason = "worker_recovery"
	}
	opts := defaultModemRebootRecoveryOptions(deviceID, reason)
	opts.delays = recoveryDelaysForReason(reason)
	opts.transportEvent = event
	if event != nil && p.transportRecovery != nil {
		if event.DeviceID == "" {
			event.DeviceID = deviceID
		}
		if !p.transportRecovery.Observe(*event) {
			logger.Debug("QMI 恢复已在运行，跳过重复调度", "device", deviceID, "reason", reason)
			return false
		}
		opts.transportEventObserved = true
	}
	if worker := p.GetWorker(deviceID); worker != nil {
		worker.RecordWatchdogEvent(WatchdogEvent{
			Layer:     HealthLayerPool,
			State:     HealthStateReprobing,
			EventType: "worker_reprobe",
			Reason:    reason,
		})
	}
	go p.runModemRebootRecovery(opts)
	return true
}

func (p *Pool) scheduleWorkerRecovery(deviceID string, reason string) {
	deviceID = strings.TrimSpace(deviceID)
	reason = strings.TrimSpace(reason)
	if p == nil || deviceID == "" {
		return
	}
	if reason == "" {
		reason = "worker_recovery"
	}
	if worker := p.GetWorker(deviceID); worker != nil {
		worker.RecordWatchdogEvent(WatchdogEvent{
			Layer:     HealthLayerPool,
			State:     HealthStateReprobing,
			EventType: "worker_reprobe",
			Reason:    reason,
		})
	}
	p.ScheduleModemRebootRecovery(deviceID, reason)
}

func (p *Pool) scheduleATDisconnectRecovery(owner workerLifecycleOwner, reason string) {
	if p == nil || owner.worker == nil || !p.workerLifecycleOwnerCurrent(owner) {
		return
	}
	deviceID := strings.TrimSpace(owner.worker.ID)
	reason = strings.TrimSpace(reason)
	if deviceID == "" {
		return
	}
	if reason == "" {
		reason = "modem_disconnect"
	}
	worker := owner.worker
	state := HealthStateRecovering
	if reason == "at_timeout_threshold" {
		state = HealthStateInvalid
	}
	worker.RecordWatchdogEvent(WatchdogEvent{
		Layer:     HealthLayerAT,
		State:     state,
		EventType: reason,
		Reason:    reason,
		Threshold: func() int {
			if reason == "at_timeout_threshold" {
				return 5
			}
			return 0
		}(),
	})
	worker.RecordWatchdogEvent(WatchdogEvent{
		Layer:     HealthLayerPool,
		State:     HealthStateReprobing,
		EventType: "worker_reprobe",
		Reason:    reason,
	})
	opts := defaultModemRebootRecoveryOptions(deviceID, reason)
	opts.removeBeforeScan = false
	opts.initialWorker = worker
	go p.runModemRebootRecovery(opts)
}

func (p *Pool) runModemRebootRecovery(opts modemRebootRecoveryOptions) {
	if p == nil || opts.deviceID == "" {
		return
	}
	if p.adbRecoveryReplaced(opts) {
		return
	}
	generation, started := p.startModemRebootRecovery(opts.deviceID)
	if !started {
		if opts.transportEventObserved && p.transportRecovery != nil {
			p.transportRecovery.Finish(opts.deviceID)
		}
		logger.Debug("模组重启恢复已在运行，跳过重复调度", "device", opts.deviceID, "reason", opts.reason)
		return
	}

	if opts.transportEvent != nil && p.transportRecovery != nil && !opts.transportEventObserved {
		if !p.transportRecovery.Observe(*opts.transportEvent) {
			p.finishModemRebootRecoveryGen(opts.deviceID, generation)
			logger.Debug("QMI 恢复已在运行，释放 modem reboot 锁并跳过", "device", opts.deviceID, "reason", opts.reason)
			return
		}
	}

	defer func() {
		if p.finishModemRebootRecoveryGen(opts.deviceID, generation) && p.transportRecovery != nil {
			p.transportRecovery.Finish(opts.deviceID)
		}
	}()
	initialControlReady := false
	if worker := p.GetWorker(opts.deviceID); worker != nil {
		initialControlReady = qmiWorkerControlReady(worker)
		worker.RecordWatchdogEvent(WatchdogEvent{
			Layer:     HealthLayerPool,
			State:     HealthStateReprobing,
			EventType: "modem_reboot_recovery_start",
			Reason:    opts.reason,
		})
	}

	hadVoWiFi := false
	if opts.restoreVoWiFi {
		hadVoWiFi = p.teardownVoWiFiForReconnect(opts.deviceID)
	}
	if p.lifecycle != nil {
		p.lifecycle.BeginRecovery(opts.deviceID, LifecyclePhaseUSBWait, opts.reason, qmiLifecycleRecoveryTTL)
	}
	if opts.removeBeforeScan {
		if err := p.removeInitialRecoveryWorker(opts); err != nil {
			logger.Debug("模组重启恢复：旧 Worker 已不存在", "device", opts.deviceID, "err", err)
			if opts.initialWorker != nil {
				logger.Warn("ADB 重启恢复停止，未继续替换 Worker", "device", opts.deviceID, "err", err)
				return
			}
		}
	}
	for round, delay := range opts.delays {
		select {
		case <-p.ctx.Done():
			return
		default:
		}
		if !p.modemRebootRecoveryStillCurrent(opts.deviceID, generation) {
			return
		}
		p.waitModemRebootRecoveryTrigger(opts.deviceID, delay)
		if !opts.removeBeforeScan && p.adbRecoveryReplaced(opts) {
			return
		}
		if p.ctx.Err() != nil || !p.modemRebootRecoveryStillCurrent(opts.deviceID, generation) {
			return
		}
		cfg, hasCfg := modemRebootRecoveryConfig(opts.deviceID)
		if !hasCfg && p.GetWorker(opts.deviceID) == nil {
			logger.Debug("模组重启恢复：设备已删除，停止恢复",
				"device", opts.deviceID,
				"round", round+1,
				"reason", opts.reason)
			return
		}
		if hasCfg && requiresQMICore(cfg) {
			cfg = p.overlayRuntimeQMIAttachment(cfg)
			decision := p.ResolveQMIRecoveryAttachment(cfg)
			if !decision.Ready {
				logger.Debug("模组重启恢复：QMI attachment 尚未可用，继续等待",
					"device", opts.deviceID,
					"round", round+1,
					"reason", decision.Reason,
					"control", strings.TrimSpace(cfg.ControlDevice))
				continue
			}
			logger.Debug("模组重启恢复：QMI attachment 可用于扫描",
				"device", opts.deviceID,
				"round", round+1,
				"reason", decision.Reason,
				"control", strings.TrimSpace(decision.Attachment.ControlPath),
				"interface", strings.TrimSpace(decision.Attachment.NetInterface))
		}
		if !p.modemRebootRecoveryStillCurrent(opts.deviceID, generation) {
			return
		}
		logger.Info(fmt.Sprintf("[%s] 模组重启恢复扫描 (第 %d/%d 轮)", opts.deviceID, round+1, len(opts.delays)))
		var err error
		if p.rescanAndReconnectForTest != nil {
			err = p.rescanAndReconnectForTest()
		} else {
			err = p.rescanAndReconnect(rescanReconnectOptions{
				targetDeviceID: opts.deviceID,
				manualReboot:   strings.TrimSpace(opts.reason) == "manual_reboot" || opts.reason == modemADBRebootReason,
			})
		}
		if err != nil {
			logger.Warn("模组重启恢复扫描失败", "device", opts.deviceID, "round", round+1, "err", err)
			continue
		}
		worker := p.GetWorker(opts.deviceID)
		if worker != nil {
			controlReadyBeforeIdentityRefresh := qmiWorkerControlReady(worker) || initialControlReady
			if err := p.refreshModemRebootRecoveredIdentity(worker, opts.reason); err != nil {
				logger.Warn("模组重启恢复后 SIM 身份未就绪，继续等待",
					"device", opts.deviceID,
					"round", round+1,
					"err", err)
				if modemRebootRecoveryShouldRebuildAfterTransportDown(worker, err) {
					logger.Warn("模组重启恢复检测到控制面传输已断开，移除 Worker 等待下一轮重新接管",
						"device", opts.deviceID,
						"round", round+1,
						"reason", opts.reason,
						"err", err)
					if removeErr := p.RemoveWorker(opts.deviceID); removeErr != nil {
						logger.Warn("模组重启恢复移除传输断开 Worker 失败",
							"device", opts.deviceID,
							"round", round+1,
							"err", removeErr)
					}
				} else if worker.isQMICoreStarting() {
					logger.Info("模组重启恢复：QMI Core 仍在启动，SIM 身份转入后台收敛",
						"device", opts.deviceID,
						"round", round+1,
						"reason", opts.reason,
						"err", err)
					p.startQMIIdentityConvergence(worker, opts.reason)
					return
				} else if modemRebootRecoveryShouldRebuildAfterReadinessFailureForWorker(opts, worker, err) {
					logger.Warn("模组重启恢复检测到半就绪 Worker，移除后等待下一轮重新接管",
						"device", opts.deviceID,
						"round", round+1,
						"reason", opts.reason,
						"err", err)
					if removeErr := p.RemoveWorker(opts.deviceID); removeErr != nil {
						logger.Warn("模组重启恢复移除半就绪 Worker 失败",
							"device", opts.deviceID,
							"round", round+1,
							"err", removeErr)
					}
				} else if controlReadyBeforeIdentityRefresh {
					logger.Info("模组重启恢复：QMI 控制面已恢复，SIM 身份转入后台收敛",
						"device", opts.deviceID,
						"round", round+1,
						"reason", opts.reason,
						"err", err)
					p.startQMIIdentityConvergence(worker, opts.reason)
					return
				}
				continue
			}
			if worker.isQMICoreStarting() && !qmiWorkerControlReady(worker) {
				logger.Info("模组重启恢复：SIM 身份已就绪，等待 QMI Core 完成后台启动",
					"device", opts.deviceID,
					"round", round+1,
					"reason", opts.reason)
				return
			}
			logger.Info("模组重启恢复成功", "device", opts.deviceID, "round", round+1)
			p.markQMIControlRecovered(worker, opts.reason)
			if hadVoWiFi {
				start := p.captureVoWiFiReadyStart(worker)
				go func() {
					if err := p.enableVoWiFiWhenReady(start, 5*time.Second, opts.reason); err != nil {
						logger.Warn("模组重启恢复后恢复 VoWiFi 失败", "device", opts.deviceID, "err", err)
					}
				}()
			}
			return
		}
	}
	if worker := p.GetWorker(opts.deviceID); worker != nil {
		worker.RecordWatchdogEvent(WatchdogEvent{
			Layer:     HealthLayerPool,
			State:     HealthStateFailed,
			EventType: "modem_reboot_recovery_exhausted",
			Reason:    opts.reason,
		})
	}
	logger.Warn("模组重启恢复多轮扫描未恢复，等待健康检查兜底", "device", opts.deviceID, "reason", opts.reason)
}
