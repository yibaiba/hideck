package device

import (
	"fmt"
	"strings"

	"github.com/yibaiba/hideck/internal/backend"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/modem"
)

func newWorkerModem(cfg config.DeviceConfig, backendMode string, profile modem.ATProfile) (*modem.Manager, error) {
	if backend.NormalizeBackendMode(backendMode) == backend.BackendMBIM {
		return modem.NewSMSAuxiliary(cfg)
	}
	return modem.NewWithATProfile(cfg, profile)
}

func newWorkerBackendStrict(deviceID, backendMode, controlDevice string, m *modem.Manager, source backend.QMISource, mbimSource backend.MBIMSource) (backend.DeviceBackend, error) {
	be, err := backend.NewBackend(backendMode, controlDevice, m, source, mbimSource)
	if err != nil {
		prefix := ""
		if id := strings.TrimSpace(deviceID); id != "" {
			prefix = fmt.Sprintf("[%s] ", id)
		}
		return nil, fmt.Errorf("%s初始化 %s 后端失败: %w", prefix, backendMode, err)
	}
	return be, nil
}

func backendUsesATRuntime(mode string) bool {
	normalized := backend.NormalizeBackendMode(mode)
	return normalized == backend.BackendAT || normalized == backend.BackendMBIM
}
