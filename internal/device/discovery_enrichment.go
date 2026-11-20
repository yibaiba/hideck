package device

import (
	"strings"
	"time"

	qmiq "github.com/iniwex5/quectel-qmi-go/pkg/qmi"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/pkg/logger"
)

var discoverQMIDevicesFn = DiscoverQMIDevices
var probeIMEIViaQMIFn = ProbeIMEIViaQMIWithOptions

type QMIDeviceEnrichOptions struct {
	EnableATProbe      bool
	ATProbeTimeout     time.Duration
	EnableQMIIMEIProbe bool
	QMIClientOptions   qmiq.ClientOptions
}

type CompatibleModemEnrichOptions struct {
	EnableATProbe      bool
	ATProbeTimeout     time.Duration
	EnableQMIIMEIProbe bool
	QMIClientOptions   qmiq.ClientOptions
}

func enrichDiscoveredQMIDeviceATFirst(dev QMIDevice, timeout time.Duration, allowQMIIMEIProbe bool) (QMIDevice, string) {
	if atPort, imei := ResolveATPortForDevice(dev.ATPort, dev.ATPorts, timeout); imei != "" {
		if atPort != "" {
			dev.ATPort = atPort
		}
		return dev, imei
	}
	return resolveDiscoveredQMIDeviceFn(dev, timeout, allowQMIIMEIProbe)
}

func atProbeTimeoutOrDefault(timeout time.Duration) time.Duration {
	if timeout > 0 {
		return timeout
	}
	return 1600 * time.Millisecond
}

// EnrichDiscoveredQMIDevice 按调用方策略补全单台静态发现到的 QMI 设备信息。
// 该流程只会在本设备 ATPorts 范围内做 AT 口探测；QMI IMEI 补读作为最后手段单独开关控制。
// QMI 身份探测失败时，再在本设备 AT 口上回退一次，避免 Sync/版本查询超时把硬件打成 degraded。
func EnrichDiscoveredQMIDevice(dev QMIDevice, opts QMIDeviceEnrichOptions) (QMIDevice, string) {
	imei := ""

	if opts.EnableATProbe {
		resolved, probedIMEI := ResolveQMIDeviceATPort(dev, opts.ATProbeTimeout)
		dev = resolved
		if probedIMEI != "" {
			imei = probedIMEI
		}
	}

	qmiAttempted := false
	if imei == "" && opts.EnableQMIIMEIProbe && strings.TrimSpace(dev.ControlPath) != "" {
		qmiAttempted = true
		if qmiIMEI, err := probeIMEIViaQMIFn(dev.ControlPath, opts.QMIClientOptions); err == nil && qmiIMEI != "" {
			imei = qmiIMEI
		} else if err != nil {
			logger.Debug("QMI IMEI 探测失败", "control_path", dev.ControlPath, "err", err)
		}
	}

	if imei == "" && qmiAttempted && !opts.EnableATProbe {
		resolved, probedIMEI := ResolveQMIDeviceATPort(dev, atProbeTimeoutOrDefault(opts.ATProbeTimeout))
		if probedIMEI != "" {
			dev = resolved
			imei = probedIMEI
			logger.Debug("QMI IMEI 探测失败，已用本设备 AT 口回退",
				"control_path", dev.ControlPath,
				"at", dev.ATPort)
		}
	}
	return dev, imei
}

// EnrichDiscoveredCompatibleModem 按调用方策略补全兼容发现结果。
// QMI IMEI 补读仅在控制口存在且调用方显式允许时才执行。
func EnrichDiscoveredCompatibleModem(dev CompatibleModem, opts CompatibleModemEnrichOptions) (CompatibleModem, string) {
	imei := ""
	if !opts.EnableATProbe {
		imei = strings.TrimSpace(dev.IMEI)
	}

	if opts.EnableATProbe {
		resolved, probedIMEI := ResolveCompatibleModemATPort(dev, opts.ATProbeTimeout)
		dev = resolved
		if probedIMEI != "" {
			imei = probedIMEI
		}
	}

	qmiAttempted := false
	if imei == "" && opts.EnableQMIIMEIProbe && strings.TrimSpace(dev.ControlPath) != "" {
		qmiAttempted = true
		if qmiIMEI, err := probeIMEIViaQMIFn(dev.ControlPath, opts.QMIClientOptions); err == nil && qmiIMEI != "" {
			imei = qmiIMEI
			dev.IMEI = qmiIMEI
		} else if err != nil {
			logger.Debug("QMI IMEI 探测失败", "control_path", dev.ControlPath, "err", err)
		}
	}

	if imei == "" && qmiAttempted && !opts.EnableATProbe {
		resolved, probedIMEI := ResolveCompatibleModemATPort(dev, atProbeTimeoutOrDefault(opts.ATProbeTimeout))
		if probedIMEI != "" {
			dev = resolved
			imei = probedIMEI
			logger.Debug("QMI IMEI 探测失败，已用本设备 AT 口回退",
				"control_path", dev.ControlPath,
				"at", dev.ATPort)
		}
	}

	if imei != "" {
		dev.IMEI = imei
	}
	return dev, imei
}

type StaticQMIDeviceIndex struct {
	byControl map[string]QMIDevice
	byUSB     map[string]QMIDevice
	byIface   map[string]QMIDevice
}

func BuildStaticQMIDeviceIndex(devices []QMIDevice) StaticQMIDeviceIndex {
	idx := StaticQMIDeviceIndex{
		byControl: map[string]QMIDevice{},
		byUSB:     map[string]QMIDevice{},
		byIface:   map[string]QMIDevice{},
	}
	for _, dev := range devices {
		if key := strings.TrimSpace(dev.ControlPath); key != "" {
			if _, ok := idx.byControl[key]; !ok {
				idx.byControl[key] = dev
			}
		}
		if key := strings.TrimSpace(dev.USBPath); key != "" {
			if _, ok := idx.byUSB[key]; !ok {
				idx.byUSB[key] = dev
			}
		}
		if key := strings.TrimSpace(dev.NetInterface); key != "" {
			if _, ok := idx.byIface[key]; !ok {
				idx.byIface[key] = dev
			}
		}
	}
	return idx
}

func (idx StaticQMIDeviceIndex) Lookup(controlPath, usbPath, iface string) (QMIDevice, bool) {
	if key := strings.TrimSpace(controlPath); key != "" {
		if dev, ok := idx.byControl[key]; ok {
			return dev, true
		}
	}
	if key := strings.TrimSpace(usbPath); key != "" {
		if dev, ok := idx.byUSB[key]; ok {
			return dev, true
		}
	}
	if key := strings.TrimSpace(iface); key != "" {
		if dev, ok := idx.byIface[key]; ok {
			return dev, true
		}
	}
	return QMIDevice{}, false
}

type WorkerDiscoveryInfo struct {
	ID          string
	ControlPath string
	USBPath     string
	Interface   string
	ATPort      string
	IMEI        string
	USBNetMode  *int
}

type workerATIdentityReader struct {
	worker *Worker
	query  func() (string, error)
}

type WorkerDiscoveryIndex struct {
	byControl        map[string]WorkerDiscoveryInfo
	byUSB            map[string]WorkerDiscoveryInfo
	byIface          map[string]WorkerDiscoveryInfo
	atIdentityByPort map[string]workerATIdentityReader
}

func BuildWorkerDiscoveryIndex(workers []*Worker, includeRuntimeStatus bool) WorkerDiscoveryIndex {
	idx := WorkerDiscoveryIndex{
		byControl:        map[string]WorkerDiscoveryInfo{},
		byUSB:            map[string]WorkerDiscoveryInfo{},
		byIface:          map[string]WorkerDiscoveryInfo{},
		atIdentityByPort: map[string]workerATIdentityReader{},
	}

	for _, worker := range workers {
		if worker == nil {
			continue
		}
		cfg := worker.Config
		info := WorkerDiscoveryInfo{
			ID:          worker.ID,
			ControlPath: strings.TrimSpace(cfg.ControlDevice),
			USBPath:     strings.TrimSpace(cfg.USBPath),
			Interface:   strings.TrimSpace(cfg.Interface),
			ATPort:      strings.TrimSpace(cfg.ATPort),
			IMEI:        strings.TrimSpace(cfg.ModemIMEI),
		}
		if worker.Modem != nil && worker.Modem.OwnsATRuntime() && worker.ResolvedATPort() != "" {
			port := worker.ResolvedATPort()
			owner := workerATIdentityReader{worker: worker, query: worker.Modem.QueryIMEI}
			idx.atIdentityByPort[port] = owner
			idx.atIdentityByPort[resolveUSBPathForScan(port)] = owner
		}
		if includeRuntimeStatus && !worker.atDeviceRules().verifyLiveIMEI {
			status := worker.GetDeviceStatus()
			if imei := strings.TrimSpace(status.IMEI); imei != "" {
				info.IMEI = imei
			}
			if info.ATPort != "" {
				v := status.USBNetMode
				info.USBNetMode = &v
			}
		}

		if info.ControlPath != "" {
			if _, ok := idx.byControl[info.ControlPath]; !ok {
				idx.byControl[info.ControlPath] = info
			}
		}
		if info.USBPath != "" {
			if _, ok := idx.byUSB[info.USBPath]; !ok {
				idx.byUSB[info.USBPath] = info
			}
		}
		if info.Interface != "" {
			if _, ok := idx.byIface[info.Interface]; !ok {
				idx.byIface[info.Interface] = info
			}
		}
	}

	return idx
}

func (idx WorkerDiscoveryIndex) Lookup(controlPath, usbPath, iface string) (WorkerDiscoveryInfo, bool) {
	if key := strings.TrimSpace(controlPath); key != "" {
		if info, ok := idx.byControl[key]; ok {
			return info, true
		}
	}
	if key := strings.TrimSpace(usbPath); key != "" {
		if info, ok := idx.byUSB[key]; ok {
			return info, true
		}
	}
	if key := strings.TrimSpace(iface); key != "" {
		if info, ok := idx.byIface[key]; ok {
			return info, true
		}
	}
	return WorkerDiscoveryInfo{}, false
}

// atScanObservation retains a static attachment even if its IMEI could not
// be verified. Only the separate verified modem result can establish identity.
type atScanObservation struct {
	usbPath         string
	atPorts         []string
	unreadableOwner *Worker
}

// ResolveLiveATIdentity handles hardware that requires a live AT IMEI.
// The caller retains its cache and transport fallback for other hardware.
func (idx WorkerDiscoveryIndex) ResolveLiveATIdentity(dev CompatibleModem, probe func(CompatibleModem) (CompatibleModem, string)) (CompatibleModem, bool) {
	resolved, observation := idx.observeLiveATIdentity(dev, probe)
	return resolved, observation != nil
}

func (idx WorkerDiscoveryIndex) observeLiveATIdentity(dev CompatibleModem, probe func(CompatibleModem) (CompatibleModem, string)) (CompatibleModem, *atScanObservation) {
	if !atDeviceRulesFor(dev.ATProfile()).verifyLiveIMEI {
		return dev, nil
	}
	observation := &atScanObservation{usbPath: dev.USBPath, atPorts: dev.ATPorts}
	if port, owner := idx.ownedATPort(dev.ATPorts); port != "" {
		dev.ATPort, dev.IMEI = port, ""
		if imei, err := owner.query(); err == nil {
			dev.IMEI = imei
		}
		if config.NormalizeIMEI(dev.IMEI) == "" {
			// Do not open a second reader while the worker owns the port.
			observation.unreadableOwner = owner.worker
		}
		return dev, observation
	}
	resolved, imei := probe(dev)
	resolved.IMEI = imei
	return resolved, observation
}

func (idx WorkerDiscoveryIndex) ownedATPort(ports []string) (string, workerATIdentityReader) {
	for _, port := range ports {
		if owner := idx.atIdentityByPort[port]; owner.query != nil {
			return port, owner
		}
		if canonical := resolveUSBPathForScan(port); canonical != port {
			if owner := idx.atIdentityByPort[canonical]; owner.query != nil {
				return port, owner
			}
		}
	}
	return "", workerATIdentityReader{}
}

type ConfiguredDeviceIndex struct {
	byControl map[string]string
	byUSB     map[string]string
	byIface   map[string]string
	byIMEI    map[string]string
}

func BuildConfiguredDeviceIndex(devices []config.DeviceConfig) ConfiguredDeviceIndex {
	idx := ConfiguredDeviceIndex{
		byControl: map[string]string{},
		byUSB:     map[string]string{},
		byIface:   map[string]string{},
		byIMEI:    map[string]string{},
	}

	for _, dev := range devices {
		id := strings.TrimSpace(dev.ID)
		if id == "" {
			continue
		}
		if key := strings.TrimSpace(dev.ControlDevice); key != "" {
			if _, ok := idx.byControl[key]; !ok {
				idx.byControl[key] = id
			}
		}
		if key := strings.TrimSpace(dev.USBPath); key != "" {
			if _, ok := idx.byUSB[key]; !ok {
				idx.byUSB[key] = id
			}
		}
		if key := strings.TrimSpace(dev.Interface); key != "" {
			if _, ok := idx.byIface[key]; !ok {
				idx.byIface[key] = id
			}
		}
		if key := config.NormalizeIMEI(dev.ModemIMEI); key != "" {
			if _, ok := idx.byIMEI[key]; !ok {
				idx.byIMEI[key] = id
			}
		}
	}

	return idx
}

func (idx ConfiguredDeviceIndex) Lookup(controlPath, usbPath, iface, imei string) string {
	if id := idx.LookupByStaticPath(controlPath, usbPath, iface); id != "" {
		return id
	}
	return idx.LookupByIMEI(imei)
}

func (idx ConfiguredDeviceIndex) LookupByStaticPath(controlPath, usbPath, iface string) string {
	if key := strings.TrimSpace(controlPath); key != "" {
		if id, ok := idx.byControl[key]; ok {
			return id
		}
	}
	if key := strings.TrimSpace(usbPath); key != "" {
		if id, ok := idx.byUSB[key]; ok {
			return id
		}
	}
	if key := strings.TrimSpace(iface); key != "" {
		if id, ok := idx.byIface[key]; ok {
			return id
		}
	}
	return ""
}

func (idx ConfiguredDeviceIndex) LookupByIMEI(imei string) string {
	if key := config.NormalizeIMEI(imei); key != "" {
		if id, ok := idx.byIMEI[key]; ok {
			return id
		}
	}
	return ""
}
