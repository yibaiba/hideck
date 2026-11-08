package vowifihost

import (
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost"
	"github.com/yibaiba/hideck/pkg/logger"
)

func (m *Manager) Instance(deviceID string) *runtimehost.Instance {
	if m == nil {
		return nil
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return nil
	}
	return m.RuntimeStore().Instance(deviceID)
}

func (m *Manager) Instances() map[string]*runtimehost.Instance {
	if m == nil {
		return nil
	}
	return m.RuntimeStore().Instances()
}

func (m *Manager) InstanceIDs() []string {
	if m == nil {
		return nil
	}
	return m.RuntimeStore().InstanceIDs()
}

func (m *Manager) Active(deviceID string) bool {
	if m == nil {
		return false
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return false
	}
	return m.RuntimeStore().Active(deviceID)
}

func (m *Manager) Starting(deviceID string) bool {
	if m == nil {
		return false
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return false
	}
	return m.RuntimeStore().Starting(deviceID)
}

func (m *Manager) State(deviceID string) (runtimehost.State, bool) {
	if m == nil {
		return runtimehost.State{}, false
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return runtimehost.State{}, false
	}
	return m.RuntimeStore().State(deviceID)
}

// ResetMWISubscriptionBackoff clears the local MWI backoff every device kept
// for one card. Network waits and MWI stop states stay, and nothing is started
// or woken here; a later start reads the reset state (hideck#102).
func (m *Manager) ResetMWISubscriptionBackoff(sameCard func(iccid string) bool) {
	if m == nil || m.subscriptionRetries == nil {
		return
	}
	resets := m.subscriptionRetries.ResetMWILocalBackoff(sameCard)
	if len(resets) == 0 {
		logger.Info("MWI 本地退避没有失败记录，无需重置")
	}
	for _, reset := range resets {
		networkWaitUntil := ""
		if !reset.NetworkWaitUntil.IsZero() {
			networkWaitUntil = reset.NetworkWaitUntil.Format(time.RFC3339)
		}
		logger.Info("已重置 MWI 本地退避；网络等待和停止状态保留，订阅不一定立即发送",
			"device", reset.Device, "cleared_failures", reset.Failures,
			"network_wait_until", networkWaitUntil, "stopped", reset.Stopped)
	}
}

// WakeMWISubscription asks the device's running runtime to re-check MWI. It
// reports false when no IMS service took the request.
func (m *Manager) WakeMWISubscription(deviceID string) bool {
	deviceID = strings.TrimSpace(deviceID)
	woken := m.Instance(deviceID).WakeMWISubscription()
	logger.Info("已请求重新检查 MWI 订阅", "device", deviceID, "runtime_woken", woken)
	return woken
}
