package device

import (
	"strings"
	"time"

	"github.com/yibaiba/hideck/internal/backend"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/pkg/logger"
)

type rescanWorkerStart struct {
	config          config.DeviceConfig
	lifecycleReason string
	vowifiReason    string
}

func (p *Pool) reconcileMatchedRescanDevice(
	opts rescanReconnectOptions,
	snapshot MatchedPair,
	fallback []config.DeviceConfig,
) {
	releaseMutation := p.AcquireManagedDeviceMutation(snapshot.Config.ID)
	defer releaseMutation()
	pair, devices, ok := p.currentMatchedRescanPair(snapshot, fallback)
	if !ok || !FreeDeviceLimitAllowsConfiguredDevice(devices, pair.Config.ID) {
		return
	}
	if pair.BackfillIMEI != "" {
		pair.Config.ModemIMEI = pair.BackfillIMEI
	}
	worker := p.GetWorker(pair.Config.ID)
	switch {
	case worker == nil:
		p.startMissingRescanWorker(opts, pair)
	case !worker.IsDeviceHealthy():
		p.rebuildUnhealthyRescanWorker(opts, pair)
	default:
		p.reconcileHealthyRescanWorker(opts, pair, worker)
	}
}

func (p *Pool) startMissingRescanWorker(opts rescanReconnectOptions, pair MatchedPair) {
	if !opts.allowWorkerMutation(pair.Config.ID) {
		return
	}
	logger.Info("检测到设备上线，自动启动", "device", pair.Config.ID, "imei", pair.Config.ModemIMEI)
	p.startRescanWorker(rescanWorkerStart{
		config:          rescanWorkerConfig(pair.Config, pair.Hardware),
		lifecycleReason: "rescan_device_online", vowifiReason: "device_recovery",
	})
}

func (p *Pool) rebuildUnhealthyRescanWorker(opts rescanReconnectOptions, pair MatchedPair) {
	if !opts.allowWorkerMutation(pair.Config.ID) {
		return
	}
	logger.Info("检测到设备恢复，尝试重新初始化", "device", pair.Config.ID, "imei", pair.Config.ModemIMEI)
	p.restartRescanWorker(rescanWorkerStart{
		config:          rescanWorkerConfig(pair.Config, pair.Hardware),
		lifecycleReason: "rescan_reinitialize", vowifiReason: "device_recovery",
	})
}

func (p *Pool) reconcileHealthyRescanWorker(
	opts rescanReconnectOptions,
	pair MatchedPair,
	worker *Worker,
) {
	hw := pair.Hardware
	hwQMI := QMIDevice{
		ControlPath: hw.ControlPath, NetInterface: hw.NetInterface,
		USBPath: hw.USBPath, ATPort: hw.ATPort,
	}
	if requiresQMICore(pair.Config) && qmiManagedAttachmentChanged(worker.Config, hwQMI) {
		if !opts.allowWorkerMutation(pair.Config.ID) {
			return
		}
		next := applyQMIManagedAttachment(pair.Config, hwQMI)
		logger.Info("检测到 QMI 设备路径变化，重建 Worker",
			"device", pair.Config.ID,
			"old_control", worker.Config.ControlDevice, "new_control", next.ControlDevice,
			"old_interface", worker.Config.Interface, "new_interface", next.Interface,
			"old_usb_path", worker.Config.USBPath, "new_usb_path", next.USBPath)
		p.restartRescanWorker(rescanWorkerStart{
			config: next, lifecycleReason: "rescan_qmi_path_change", vowifiReason: "device_qmi_path_change",
		})
		return
	}
	usbChanged := atDeviceRulesFor(hw.ATProfile()).verifyLiveIMEI && hw.USBPath != "" && hw.USBPath != worker.Config.USBPath
	portChanged := hw.ATPort != "" && hw.ATPort != worker.Config.ATPort
	if (!usbChanged && !portChanged) || !opts.allowWorkerMutation(pair.Config.ID) {
		return
	}
	logger.Info("检测到设备端口变化，重建 Worker",
		"device", pair.Config.ID, "old_port", worker.Config.ATPort, "new_port", hw.ATPort)
	p.restartRescanWorker(rescanWorkerStart{
		config:          rescanWorkerConfig(pair.Config, hw),
		lifecycleReason: "rescan_port_change", vowifiReason: "device_port_change",
	})
}

func rescanWorkerConfig(cfg config.DeviceConfig, hw CompatibleModem) config.DeviceConfig {
	if requiresQMICore(cfg) {
		return applyQMIManagedAttachment(cfg, QMIDevice{
			ControlPath: hw.ControlPath, NetInterface: hw.NetInterface,
			USBPath: hw.USBPath, ATPort: hw.ATPort,
		})
	}
	if hw.NetInterface != "" {
		cfg.Interface = hw.NetInterface
	}
	if hw.USBPath != "" {
		cfg.USBPath = hw.USBPath
	}
	cfg.ControlDevice = strings.TrimSpace(hw.ControlPath)
	cfg.QMIDevice = strings.TrimSpace(hw.ControlPath)
	cfg.ATPort = hw.ATPort
	cfg.ManagePort = hw.ATPort
	return cfg
}

func (p *Pool) restartRescanWorker(request rescanWorkerStart) {
	p.beginRescanWorkerStart(request)
	p.teardownVoWiFiForReconnect(request.config.ID)
	if err := p.RemoveWorker(request.config.ID); err != nil {
		logger.Warn("重扫时移除旧 Worker 失败",
			"device", request.config.ID, "reason", request.lifecycleReason, "err", err)
		return
	}
	p.addRescanWorker(request)
}

func (p *Pool) startRescanWorker(request rescanWorkerStart) {
	p.beginRescanWorkerStart(request)
	p.addRescanWorker(request)
}

func (p *Pool) beginRescanWorkerStart(request rescanWorkerStart) {
	if p.lifecycle != nil {
		p.lifecycle.BeginRecovery(
			request.config.ID,
			LifecyclePhaseWorkerStarting,
			request.lifecycleReason,
			qmiLifecycleRecoveryTTL,
		)
	}
}

func (p *Pool) addRescanWorker(request rescanWorkerStart) {
	worker, err := p.AddWorkerFromConfig(withConnectHoldRF(request.config))
	if err != nil {
		logger.Warn("重扫时启动设备失败",
			"device", request.config.ID, "reason", request.lifecycleReason, "err", err)
		return
	}
	if !worker.Config.VoWiFiEnabled {
		return
	}
	start := p.captureVoWiFiReadyStart(worker)
	go func() {
		if err := p.enableVoWiFiWhenReady(start, 5*time.Second, request.vowifiReason); err != nil {
			logger.Warn("设备恢复后自动重启 VoWiFi 失败",
				"device", request.config.ID, "reason", request.vowifiReason, "err", err)
		}
	}()
}

// currentMatchedRescanPair reloads configuration after acquiring the managed
// mutation lease. A scan snapshot must not recreate a deleted or rebound ID.
func (p *Pool) currentMatchedRescanPair(
	snapshot MatchedPair,
	fallback []config.DeviceConfig,
) (MatchedPair, []config.DeviceConfig, bool) {
	devices := p.managedDevicesAfterLease(fallback)
	pair, ok := matchConfiguredRescanPair(snapshot, devices)
	return pair, devices, ok
}

func matchConfiguredRescanPair(snapshot MatchedPair, devices []config.DeviceConfig) (MatchedPair, bool) {
	current, ok := configuredDeviceByID(devices, snapshot.Config.ID)
	if !ok || resolvedBackendMode(current) == backend.BackendPCSC {
		return MatchedPair{}, false
	}
	resolved := ResolveDeviceIdentities([]CompatibleModem{snapshot.Hardware}, []config.DeviceConfig{current})
	if len(resolved.Matched) != 1 {
		return MatchedPair{}, false
	}
	return resolved.Matched[0], true
}

func configuredDeviceByID(devices []config.DeviceConfig, deviceID string) (config.DeviceConfig, bool) {
	deviceID = strings.TrimSpace(deviceID)
	for _, candidate := range devices {
		if strings.TrimSpace(candidate.ID) == deviceID {
			return candidate, true
		}
	}
	return config.DeviceConfig{}, false
}

func (p *Pool) managedDevicesAfterLease(fallback []config.DeviceConfig) []config.DeviceConfig {
	devices := p.managedDevices()
	if p.managedDeviceSource == nil {
		return append([]config.DeviceConfig(nil), fallback...)
	}
	return devices
}

func (p *Pool) reconcileOfflineRescanDevice(
	opts rescanReconnectOptions,
	snapshot config.DeviceConfig,
	hardware []CompatibleModem,
	atObservations []atScanObservation,
	fallback []config.DeviceConfig,
) {
	releaseMutation := p.AcquireManagedDeviceMutation(snapshot.ID)
	defer releaseMutation()
	devices := p.managedDevicesAfterLease(fallback)
	current, ok := configuredDeviceByID(devices, snapshot.ID)
	if !ok || resolvedBackendMode(current) == backend.BackendPCSC || !opts.allowWorkerMutation(current.ID) {
		return
	}
	resolved := ResolveDeviceIdentities(hardware, []config.DeviceConfig{current})
	if len(resolved.Offline) != 1 || !FreeDeviceLimitAllowsConfiguredDevice(devices, current.ID) {
		return
	}
	worker := p.GetWorker(current.ID)
	if worker == nil {
		return
	}
	healthy := worker.IsDeviceHealthy()
	if healthy && worker.atDeviceRules().verifyLiveIMEI && config.IMEIMatches(worker.Config.ModemIMEI, current.ModemIMEI) {
		usb := strings.TrimSpace(worker.Config.USBPath)
		port := worker.ResolvedATPort()
		for _, candidate := range atObservations {
			if candidate.unreadableOwner == worker && usb != "" && candidate.usbPath == usb &&
				containsPort(candidate.atPorts, port) {
				// Keep the existing owner until the next scan can verify its IMEI.
				return
			}
		}
	}
	logger.Info("检测到设备离线，清理 Worker 以便后续重建",
		"device", current.ID, "imei", current.ModemIMEI,
		"was_healthy", healthy)
	p.teardownVoWiFiForReconnect(current.ID)
	if p.lifecycle != nil {
		p.lifecycle.BeginRecovery(current.ID, LifecyclePhaseUSBWait, "rescan_device_missing", qmiLifecycleRecoveryTTL)
	}
	if err := p.RemoveWorker(current.ID); err != nil {
		logger.Warn("清理离线 Worker 失败", "device", current.ID, "err", err)
	}
}
