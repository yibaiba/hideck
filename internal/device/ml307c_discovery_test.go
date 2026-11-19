package device

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yibaiba/hideck/internal/backend"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/modem"
)

func TestML307CDiscoveryAndRebind(t *testing.T) {
	usbPath := filepath.Join(t.TempDir(), "1-1")
	for _, node := range []string{"1-1:1.2/ttyUSB8", "1-1:1.3/ttyUSB2", "1-1:1.4/ttyUSB3"} {
		if err := os.MkdirAll(filepath.Join(usbPath, node), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range map[string]string{"idVendor": "2ecc\n", "idProduct": "3012\n"} {
		if err := os.WriteFile(filepath.Join(usbPath, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	dev, ok := discoverFallbackOne(usbPath)
	if !ok || dev.Mode != "at" || dev.NetworkCapable || dev.ControlPath != "" || dev.ATProfile() != modem.ATProfileML307C {
		t.Fatalf("discovery = %+v, %v", dev, ok)
	}
	if dev.ATPort != "/dev/ttyUSB8" || !slices.Equal(dev.ATPorts, []string{"/dev/ttyUSB8", "/dev/ttyUSB2"}) {
		t.Fatalf("ATPort=%q ATPorts=%v", dev.ATPort, dev.ATPorts)
	}

	const imei = "490154203237518"
	owned := WorkerDiscoveryIndex{atIdentityByPort: map[string]workerATIdentityReader{
		"/dev/ttyUSB8": {query: func() (string, error) { return imei, nil }},
	}}
	unexpectedProbe := func(raw CompatibleModem) (CompatibleModem, string) {
		t.Fatal("unexpected independent AT probe")
		return raw, ""
	}
	active, ok := owned.ResolveLiveATIdentity(dev, unexpectedProbe)
	if !ok || active.IMEI != imei {
		t.Fatalf("owned IMEI = %q, %v", active.IMEI, ok)
	}
	owned.atIdentityByPort["/dev/ttyUSB8"] = workerATIdentityReader{
		query: func() (string, error) { return "", errors.New("old tty closed") },
	}
	dev.IMEI = imei
	active, ok = owned.ResolveLiveATIdentity(dev, unexpectedProbe)
	if !ok || active.IMEI != "" {
		t.Fatalf("stale owned tty = %q, %v", active.IMEI, ok)
	}
	ordinary := dev
	ordinary.VendorID, ordinary.ProductID = 0, 0
	if _, handled := owned.ResolveLiveATIdentity(ordinary, unexpectedProbe); handled {
		t.Fatal("ordinary AT must retain the caller's existing fallback")
	}
	if active, handled := (WorkerDiscoveryIndex{}).ResolveLiveATIdentity(dev, func(raw CompatibleModem) (CompatibleModem, string) {
		return raw, ""
	}); !handled || active.IMEI != "" {
		t.Fatalf("unowned failed read retained a cached IMEI: %q, %v", active.IMEI, handled)
	}

	origProbe := probeProfileIMEICachedFn
	t.Cleanup(func() { probeProfileIMEICachedFn = origProbe })
	var probed []string
	probeProfileIMEICachedFn = func(profile modem.ATProfile, _ context.Context, port string, _ time.Duration) (string, error) {
		if profile != modem.ATProfileML307C {
			t.Fatalf("IMEI probe profile = %v", profile)
		}
		probed = append(probed, port)
		if port == "/dev/ttyUSB2" {
			return imei, nil
		}
		return "", errors.New("no IMEI")
	}

	rebound, readIMEI := ResolveCompatibleModemATPort(dev, time.Second)
	if readIMEI != imei || !slices.Equal(probed, dev.ATPorts) {
		t.Fatalf("rebind IMEI = %q, ports = %v", readIMEI, probed)
	}
	cfg := rescanWorkerConfig(config.DeviceConfig{ID: "c", DeviceBackend: "at", ModemIMEI: imei, ATPort: "/dev/ttyUSB0"}, rebound)
	if cfg.USBPath != usbPath || cfg.ATPort != "/dev/ttyUSB2" || cfg.ManagePort != cfg.ATPort {
		t.Fatalf("rebind USBPath=%q ATPort=%q ManagePort=%q", cfg.USBPath, cfg.ATPort, cfg.ManagePort)
	}

	if err := os.RemoveAll(filepath.Join(usbPath, "1-1:1.2")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(usbPath, "1-1:1.3")); err != nil {
		t.Fatal(err)
	}
	if _, ok := discoverFallbackOne(usbPath); ok {
		t.Fatal("Diag alone must not be discovered as an ML307C AT port")
	}
}

func TestML307CRescanPreservesAndRebindsDiscoveredDevice(t *testing.T) {
	const imei = "490154203237518"
	const oldPort = "/dev/ttyUSB8"
	cfg := config.DeviceConfig{
		ID: "ml307c", DeviceBackend: backend.BackendAT, ESIMTransport: config.ESIMTransportAT,
		ModemIMEI: imei, USBPath: "/sys/bus/usb/devices/1-1", ATPort: oldPort, ManagePort: oldPort,
	}
	original := CompatibleModem{
		USBPath: cfg.USBPath, VendorID: 0x2ecc, ProductID: 0x3012,
		ATPort: oldPort, ATPorts: []string{oldPort}, IMEI: imei, Mode: "at", TransportType: backend.BackendAT,
	}
	other := CompatibleModem{USBPath: "/sys/bus/usb/devices/1-2", ATPort: "/dev/ttyUSB4", Mode: "at"}
	candidates := []CompatibleModem{original, other}
	prevQMI, prevFallback, prevEnrich := discoverQMIDevicesFn, discoverFallbackModemsFn, resolveDiscoveredCompatibleModemFn
	t.Cleanup(func() {
		discoverQMIDevicesFn, discoverFallbackModemsFn, resolveDiscoveredCompatibleModemFn = prevQMI, prevFallback, prevEnrich
	})
	discoverQMIDevicesFn = func() ([]QMIDevice, error) { return nil, nil }
	discoverFallbackModemsFn = func() ([]CompatibleModem, error) { return candidates, nil }
	resolveDiscoveredCompatibleModemFn = func(dev CompatibleModem, _ time.Duration) (CompatibleModem, string) {
		if dev.ATProfile() != modem.ATProfileML307C {
			t.Fatalf("explicit port configuration triggered an unrelated AT probe: %+v", dev)
		}
		return dev, dev.IMEI
	}

	p := NewPool(&config.Config{Devices: []config.DeviceConfig{cfg}})
	t.Cleanup(p.cancel)
	// Use the existing fake health backend so the rescan needs no real AT owner.
	worker := &Worker{ID: cfg.ID, Config: cfg, Backend: &workerStatusBackendStub{mode: backend.BackendQMI}}
	p.workers[cfg.ID] = worker
	if err := p.RescanAndReconnect(); err != nil || p.GetWorker(cfg.ID) != worker {
		t.Fatalf("rescan retired an unchanged, healthy ML307C: %v", err)
	}

	// The offline-worker recovery must select by IMEI even when another
	// ML307C gets the previous USB location and tty number.
	delete(p.workers, cfg.ID)
	reused := original
	reused.IMEI = "490154203237526"
	moved := original
	moved.USBPath = "/sys/bus/usb/devices/2-1"
	moved.ATPort = "/dev/ttyUSB13"
	moved.ATPorts = []string{moved.ATPort}
	candidates = []CompatibleModem{reused, moved, other}
	managed := p.managedDevices()
	hardware, _ := p.collectRescanHardware(nil, BuildWorkerDiscoveryIndex(p.GetAllWorkers(), false), configuredDevicesATDiscovery(managed))
	resolved := ResolveDeviceIdentities(hardware, managed)
	if len(resolved.Matched) != 1 || len(resolved.Offline) != 0 {
		t.Fatalf("rescan did not rediscover an offline ML307C: %+v", resolved)
	}
	pair := resolved.Matched[0]
	rebound := rescanWorkerConfig(pair.Config, pair.Hardware)
	if rebound.ModemIMEI != imei || rebound.USBPath != moved.USBPath || rebound.ATPort != moved.ATPort || rebound.ManagePort != moved.ATPort {
		t.Fatalf("rescan selected the wrong attachment: %+v", rebound)
	}
}

func TestML307CBootstrapRejectsUnreadableIdentityOnConfiguredPort(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	port := filepath.Join(dir, "ttyUSB8")
	if err := os.WriteFile(port, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "by-id")
	if err := os.Symlink(port, alias); err != nil {
		t.Fatal(err)
	}
	cfg := config.DeviceConfig{
		ID: "ml307c", DeviceBackend: backend.BackendAT, ESIMTransport: config.ESIMTransportAT,
		ModemIMEI: "490154203237518", ATPort: alias, ManagePort: alias,
	}

	prevFallback, prevEnrich := discoverFallbackModemsFn, resolveDiscoveredCompatibleModemFn
	t.Cleanup(func() { discoverFallbackModemsFn, resolveDiscoveredCompatibleModemFn = prevFallback, prevEnrich })
	discoverFallbackModemsFn = func() ([]CompatibleModem, error) {
		return []CompatibleModem{
			{USBPath: "/sys/bus/usb/devices/1-1", VendorID: 0x2ecc, ProductID: 0x3012, ATPort: port, ATPorts: []string{port}, Mode: "at"},
			{USBPath: "/sys/bus/usb/devices/1-2", ATPort: "/dev/ttyUSB4", Mode: "at"},
		}, nil
	}
	probes := 0
	resolveDiscoveredCompatibleModemFn = func(dev CompatibleModem, _ time.Duration) (CompatibleModem, string) {
		if dev.ATProfile() != modem.ATProfileML307C {
			t.Fatalf("bootstrap probed an unrelated AT device: %+v", dev)
		}
		probes++
		return dev, ""
	}

	p := NewPool(&config.Config{Devices: []config.DeviceConfig{cfg}})
	t.Cleanup(p.cancel)
	worker, err := p.AddWorkerFromConfig(cfg)
	if worker != nil || err == nil || !strings.Contains(err.Error(), "未找到匹配的非 QMI 硬件") || probes != 1 {
		t.Fatalf("unverified ML307C port did not block startup: worker=%v err=%v probes=%d", worker, err, probes)
	}
}

func TestML307CRescanKeepsHealthyOwnerWhenIMEIUnavailable(t *testing.T) {
	const port = "/dev/ttyUSB8"
	const usbPath = "/sys/bus/usb/devices/1-1"
	cfg := config.DeviceConfig{ID: "ml307c", DeviceBackend: backend.BackendAT, ModemIMEI: "490154203237518"}
	p := NewPool(&config.Config{Devices: []config.DeviceConfig{cfg}})
	t.Cleanup(p.cancel)
	workerCfg := cfg
	workerCfg.USBPath = usbPath
	workerCfg.ATPort = port
	m, err := modem.NewWithATProfile(workerCfg, modem.ATProfileML307C)
	if err != nil {
		t.Fatal(err)
	}
	// The unopened manager rejects the identity query; the health stub keeps
	// the failure isolated to this single command for the test.
	worker := &Worker{ID: cfg.ID, Config: workerCfg, Modem: m,
		Backend: &workerStatusBackendStub{mode: backend.BackendQMI}, stop: make(chan struct{})}
	p.workers[cfg.ID] = worker

	prevQMI, prevFallback, prevEnrich := discoverQMIDevicesFn, discoverFallbackModemsFn, resolveDiscoveredCompatibleModemFn
	t.Cleanup(func() {
		discoverQMIDevicesFn, discoverFallbackModemsFn, resolveDiscoveredCompatibleModemFn = prevQMI, prevFallback, prevEnrich
	})
	discoverQMIDevicesFn = func() ([]QMIDevice, error) { return nil, nil }
	discoverFallbackModemsFn = func() ([]CompatibleModem, error) {
		return []CompatibleModem{{
			USBPath: usbPath, VendorID: 0x2ecc, ProductID: 0x3012,
			ATPort: port, ATPorts: []string{port}, Mode: "at",
		}}, nil
	}
	resolveDiscoveredCompatibleModemFn = func(dev CompatibleModem, _ time.Duration) (CompatibleModem, string) {
		t.Fatalf("rescan attempted an independent identity probe on the owned AT port: %+v", dev)
		return dev, ""
	}

	if err := p.RescanAndReconnect(); err != nil || p.GetWorker(cfg.ID) != worker {
		t.Fatalf("rescan retired the healthy owner after its identity query failed: %v", err)
	}
}
