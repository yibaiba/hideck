package device

import (
	"context"
	"fmt"
	"time"

	qmimanager "github.com/iniwex5/quectel-qmi-go/pkg/manager"
	"github.com/yibaiba/hideck/internal/vowifihost"
	"github.com/yibaiba/hideck/pkg/logger"
)

type voWiFiReadyStart struct {
	owner               workerLifecycleOwner
	lifecycleGeneration uint64
}

func waitForCondition(ctx context.Context, interval time.Duration, check func() bool) error {
	if check() {
		return nil
	}
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if check() {
				return nil
			}
		}
	}
}

func (p *Pool) waitWorkerReady(deviceID string, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(p.ctx, timeout)
	defer cancel()
	return waitForCondition(waitCtx, 200*time.Millisecond, func() bool {
		w := p.GetWorker(deviceID)
		if w == nil {
			return false
		}
		return w.IsDeviceHealthy()
	})
}

func (p *Pool) waitRadioRecoveryReady(deviceID string, timeout time.Duration) error {
	w := p.GetWorker(deviceID)
	if w == nil {
		return fmt.Errorf("设备 %s 不存在", deviceID)
	}
	waitCtx, cancel := context.WithTimeout(p.ctx, timeout)
	defer cancel()

	if b, ok := w.Backend.(interface {
		GetUIMReadiness(context.Context) (qmimanager.UIMReadiness, error)
	}); ok {
		return waitForCondition(waitCtx, 500*time.Millisecond, func() bool {
			rdy, err := b.GetUIMReadiness(waitCtx)
			if err != nil {
				return false
			}
			return rdy.Reason == qmimanager.UIMReadinessReady
		})
	}
	if w.QMICore != nil {
		return w.QMICore.WaitIdentityReady(waitCtx)
	}
	if w.Modem != nil {
		if !w.Modem.WaitReady(timeout) {
			return context.DeadlineExceeded
		}
		return nil
	}
	return nil
}

func (p *Pool) waitQMICoreReady(deviceID string, timeout time.Duration) error {
	return p.waitQMICoreReadyContext(p.ctx, deviceID, timeout)
}

func (p *Pool) waitQMICoreReadyContext(ctx context.Context, deviceID string, timeout time.Duration) error {
	w := p.GetWorker(deviceID)
	if w == nil {
		return fmt.Errorf("设备 %s 不存在", deviceID)
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if b, ok := w.Backend.(interface {
		GetUIMReadiness(context.Context) (qmimanager.UIMReadiness, error)
	}); ok {
		return waitForCondition(waitCtx, 500*time.Millisecond, func() bool {
			rdy, err := b.GetUIMReadiness(waitCtx)
			if err != nil {
				return false
			}
			return rdy.Reason == qmimanager.UIMReadinessReady
		})
	}
	if w.QMICore != nil {
		return w.QMICore.WaitIdentityReady(waitCtx)
	}
	return nil
}

func (p *Pool) WaitQMICoreReady(deviceID string, timeout time.Duration) error {
	return p.waitQMICoreReady(deviceID, timeout)
}

func (p *Pool) waitQMIControlReady(deviceID string, timeout time.Duration) error {
	w := p.GetWorker(deviceID)
	if w == nil {
		return fmt.Errorf("设备 %s 不存在", deviceID)
	}
	waitCtx, cancel := context.WithTimeout(p.ctx, timeout)
	defer cancel()

	if b, ok := w.Backend.(interface {
		GetUIMReadiness(context.Context) (qmimanager.UIMReadiness, error)
	}); ok {
		return waitForCondition(waitCtx, 500*time.Millisecond, func() bool {
			rdy, err := b.GetUIMReadiness(waitCtx)
			if err != nil {
				return false
			}
			return rdy.ControlReady
		})
	}
	if w.QMICore != nil {
		return w.QMICore.WaitControlReady(waitCtx)
	}
	return nil
}

func (p *Pool) WaitQMIControlReady(deviceID string, timeout time.Duration) error {
	return p.waitQMIControlReady(deviceID, timeout)
}

func (p *Pool) WaitWorkerReady(deviceID string, timeout time.Duration) error {
	return p.waitWorkerReady(deviceID, timeout)
}

func (p *Pool) WorkerExists(deviceID string) bool {
	return p.GetWorker(deviceID) != nil
}

func (p *Pool) IsSwitching(deviceID string) bool {
	return p.IsESIMSwitching(deviceID)
}

func (p *Pool) captureVoWiFiReadyStart(worker *Worker) voWiFiReadyStart {
	if p == nil || worker == nil {
		return voWiFiReadyStart{}
	}
	owner := workerLifecycleOwner{worker: worker, generation: worker.generation}
	if !p.workerLifecycleOwnerCurrent(owner) {
		return voWiFiReadyStart{}
	}
	generation := p.voWiFiHost().CurrentLifecycleGeneration(worker.ID)
	if generation == 0 {
		generation = p.voWiFiHost().NextLifecycleGeneration(worker.ID)
	}
	return voWiFiReadyStart{owner: owner, lifecycleGeneration: generation}
}

// enableVoWiFiWhenReady waits for the captured Worker, then submits an
// ownership-bound lifecycle command. Removal invalidates its generation.
func (p *Pool) enableVoWiFiWhenReady(start voWiFiReadyStart, timeout time.Duration, reason string) error {
	if !p.workerLifecycleOwnerCurrent(start.owner) || start.lifecycleGeneration == 0 {
		return fmt.Errorf("等待启动 VoWiFi 的设备已被移除或替换")
	}
	deviceID := start.owner.worker.ID
	if err := p.waitQMICoreReady(deviceID, timeout); err != nil {
		return fmt.Errorf("等待设备 %s 身份就绪失败(%s): %w", deviceID, reason, err)
	}
	if err := p.waitWorkerReady(deviceID, timeout); err != nil {
		return fmt.Errorf("等待设备 %s 就绪失败(%s): %w", deviceID, reason, err)
	}
	if !p.workerLifecycleOwnerCurrent(start.owner) {
		return fmt.Errorf("设备 %s 就绪期间已被移除或替换", deviceID)
	}
	if err := p.enableVoWiFi(deviceID, start.lifecycleGeneration); err != nil {
		return err
	}
	if !p.workerLifecycleOwnerCurrent(start.owner) ||
		p.voWiFiHost().CurrentLifecycleGeneration(deviceID) != start.lifecycleGeneration {
		return fmt.Errorf("设备 %s 的 VoWiFi 自动启动已因设备生命周期变化而失效", deviceID)
	}
	return nil
}

func (p *Pool) EnableVoWiFi(deviceID string) error {
	return p.enableVoWiFi(deviceID, 0)
}

func (p *Pool) enableVoWiFi(deviceID string, lifecycleGeneration uint64) error {
	if err := p.CheckMBNIdle(deviceID); err != nil {
		return err
	}
	if p.IsESIMSwitching(deviceID) {
		return fmt.Errorf("设备 %s 正在切卡，暂不允许启动 VoWiFi", deviceID)
	}
	if w := p.GetWorker(deviceID); w != nil {
		if err := w.CheckPhoneModeSupported(w.Config.PhoneMode); err != nil {
			return err
		}
		if IsNativeVoLTEMode(w.Config.PhoneMode) {
			return p.EnableNativeVoLTE(deviceID)
		}
		if IsModemVoiceMode(w.Config.PhoneMode) {
			return fmt.Errorf("设备 %s: %w", deviceID, ErrModemVoiceSoftwareIMS)
		}
	}
	p.stopNativeVoLTE(deviceID, "enable_vowifi")
	if err := p.stopModemVoice(deviceID); err != nil {
		return err
	}
	if w := p.GetWorker(deviceID); w != nil && w.Config.PhoneMode == "cellular" {
		if w.Config.AirplaneEnabled {
			logger.Info("蜂窝飞行：待机不建 SWu 隧道", "device", deviceID)
			return p.StopVoWiFiRuntimeForCellularIdle(deviceID)
		}
		if w.Config.DataStrategy != "always" {
			if nc := w.NetworkController(); nc == nil || !nc.IsConnected() {
				logger.Info("蜂窝 on_demand：待机不建 SWu 隧道，拨号时再连数据", "device", deviceID)
				return p.StopVoWiFiRuntimeForCellularIdle(deviceID)
			}
		} else if err := p.EnsureCellularData(p.ctx, deviceID); err != nil {
			return err
		}
	}
	if lifecycleGeneration == 0 {
		return p.voWiFiHost().Enable(p.ctx, deviceID)
	}
	return p.voWiFiHost().SubmitLifecycle(p.ctx, vowifihost.LifecycleCommand{
		DeviceID:   deviceID,
		Kind:       vowifihost.LifecycleCommandEnable,
		Reason:     "enable_when_ready",
		Generation: lifecycleGeneration,
	})
}
