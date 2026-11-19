package device

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

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
	owned := WorkerDiscoveryIndex{atIdentityByPort: map[string]func() (string, error){
		"/dev/ttyUSB8": func() (string, error) { return imei, nil },
	}}
	active, ok := owned.ReadOwnedATIdentity(dev)
	if !ok || active.IMEI != imei {
		t.Fatalf("owned IMEI = %q, %v", active.IMEI, ok)
	}
	owned.atIdentityByPort["/dev/ttyUSB8"] = func() (string, error) { return "", errors.New("old tty closed") }
	dev.IMEI = imei
	active, ok = owned.ReadOwnedATIdentity(dev)
	if !ok || active.IMEI != "" {
		t.Fatalf("stale owned tty = %q, %v", active.IMEI, ok)
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
