package device

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
	"github.com/yibaiba/hideck/internal/cardpolicy"
	"github.com/yibaiba/hideck/pkg/logger"
)

// errCellularOnDemandIdle means the software IMS/SWu stack must stay down until
// a call actually needs it. Recover must treat this as a skip, not a failure.
var errCellularOnDemandIdle = errors.New("蜂窝 on_demand 待机不建隧道")

func isCellularOnDemandIdleError(err error) bool {
	return errors.Is(err, errCellularOnDemandIdle)
}

func isCellularOnDemandIdle(w *Worker, pol cardpolicy.Policy) bool {
	mode, strategy, _ := cellularRecoverFields(w, pol)
	return mode == "cellular" && strategy != "always"
}

// cellularSoftwarePhoneHeld 表示蜂窝软件电话保持策略开启，但现在不能建 IMS：
// 飞行关着射频，或 on_demand 待机还没到拨号。
func cellularSoftwarePhoneHeld(w *Worker, pol cardpolicy.Policy) bool {
	mode, strategy, airplane := cellularRecoverFields(w, pol)
	if mode != "cellular" {
		return false
	}
	return airplane || strategy != "always"
}

func cellularRecoverFields(w *Worker, pol cardpolicy.Policy) (mode, strategy string, airplane bool) {
	mode = strings.TrimSpace(pol.PhoneMode)
	strategy = strings.TrimSpace(pol.DataStrategy)
	airplane = pol.AirplaneEnabled
	if w != nil {
		if m := strings.TrimSpace(w.Config.PhoneMode); m != "" {
			mode = m
		}
		if s := strings.TrimSpace(w.Config.DataStrategy); s != "" {
			strategy = s
		}
		if w.Config.AirplaneEnabled {
			airplane = true
		}
	}
	return mode, strategy, airplane
}

// SetVoiceGateway 注入 VoWiFi 语音网关，用于优先走 IMS 外呼/挂断路径。
func (p *Pool) SetVoiceGateway(g *voicehost.Gateway) {
	p.mu.Lock()
	p.voiceGateway = g
	p.mu.Unlock()
	p.voWiFiHost().ConfigureRuntimeDependencies(g, vowifiDeliveryStore{}, poolVoWiFiRuntimeDispatcher{pool: p})
}

func (p *Pool) StopVoWiFiRuntimeForCellularIdle(deviceID string) error {
	if p == nil {
		return nil
	}
	p.clearDesiredVoWiFiRecoverState(deviceID)
	stopCtx, cancel := p.voWiFiStopContext()
	defer cancel()
	return p.voWiFiHost().StopOrDrain(stopCtx, deviceID, "cellular_on_demand_idle", true)
}

func (p *Pool) EnsureCellularData(ctx context.Context, deviceID string) error {
	w := p.GetWorker(deviceID)
	if w == nil {
		return fmt.Errorf("设备 %s 不存在", deviceID)
	}
	if w.Config.AirplaneEnabled {
		return fmt.Errorf("飞行模式已开启，无法开启蜂窝数据")
	}
	nc := w.NetworkController()
	if nc == nil {
		return fmt.Errorf("设备 %s 没有蜂窝数据控制器", deviceID)
	}
	if !nc.IsConnected() {
		logger.Info("蜂窝模式：开启数据连接", "device", deviceID)
		if err := w.StartNetwork(); err != nil {
			return fmt.Errorf("开启蜂窝数据失败: %w", err)
		}
	}
	if ctx == nil {
		ctx = p.ctx
	}
	waitCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := waitForCondition(waitCtx, 500*time.Millisecond, nc.IsConnected); err != nil {
		return fmt.Errorf("等待蜂窝数据连接超时: %w", err)
	}
	return nil
}

func (p *Pool) PrepareCellularCall(ctx context.Context, deviceID string) error {
	w := p.GetWorker(deviceID)
	if w == nil || w.Config.PhoneMode != "cellular" {
		return nil
	}
	if w.Config.AirplaneEnabled {
		return fmt.Errorf("飞行模式已开启，无法拨号")
	}
	if err := p.EnsureCellularData(ctx, deviceID); err != nil {
		return err
	}
	if p.IsVoWiFiActive(deviceID) {
		return nil
	}
	logger.Info("蜂窝模式：拨号前启动软件 IMS", "device", deviceID)
	if err := p.EnableVoWiFi(deviceID); err != nil {
		return fmt.Errorf("蜂窝模式启动软件 IMS 失败: %w", classifyCellularIMSError(err))
	}
	waitCtx := ctx
	var cancel context.CancelFunc
	if ctx == nil {
		waitCtx, cancel = context.WithTimeout(p.ctx, 45*time.Second)
		defer cancel()
	}
	return p.WaitVoWiFiIMSReady(waitCtx, deviceID)
}

func (p *Pool) WaitVoWiFiIMSReady(ctx context.Context, deviceID string) error {
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, ok := p.GetVoWiFiRuntimeState(deviceID)
		if ok && state.IMSReady {
			return nil
		}
		if ok && state.LastError != "" && (state.IMSState == "failed" || state.SessionState == "error") {
			return fmt.Errorf("软件 IMS 启动失败: %s", state.LastError)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待软件 IMS 就绪: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// GetVoiceGateway 返回绑定的 VoiceGateway 实例
func (p *Pool) GetVoiceGateway() *voicehost.Gateway {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.voiceGateway
}

// SimulateCallWithCellularData wraps voiceGW.SimulateCall with data connect
// on/off for on_demand cellular mode. If the device is in cellular mode with
// DataStrategy=on_demand, it connects data before the call and disconnects
// after. Otherwise it calls SimulateCall directly.
func (p *Pool) SimulateCallWithCellularData(
	ctx context.Context,
	deviceID string,
	req voicehost.SimulateCallRequest,
) (*voicehost.SimulateCallResult, error) {
	w := p.GetWorker(deviceID)
	checkOwner := p.outboundOwnerCheck(w)
	if err := p.AuthorizeOutboundCall(ctx, deviceID, req.Callee); err != nil {
		return nil, err
	}
	cellular := w != nil && w.Config.PhoneMode == "cellular"
	onDemand := cellular && w.Config.DataStrategy != "always"

	if cellular {
		if err := p.EnsureCellularData(ctx, deviceID); err != nil {
			return nil, err
		}
		if !p.IsVoWiFiActive(deviceID) {
			logger.Info("蜂窝模式：数据已连通，启动软件 IMS", "device", deviceID)
			if err := p.EnableVoWiFi(deviceID); err != nil {
				return nil, fmt.Errorf("蜂窝模式启动软件 IMS 失败: %w", classifyCellularIMSError(err))
			}
			waitCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			if err := p.WaitVoWiFiIMSReady(waitCtx, deviceID); err != nil {
				return nil, err
			}
		}
	}

	voiceGW := p.GetVoiceGateway()
	if voiceGW == nil {
		return nil, fmt.Errorf("voice gateway is unavailable")
	}
	if err := checkOwner(); err != nil {
		return nil, err
	}
	result, err := voiceGW.SimulateCall(ctx, deviceID, req)

	if onDemand {
		p.cleanupOnDemandCellularCall(w, checkOwner)
	}

	return result, err
}

func (p *Pool) cleanupOnDemandCellularCall(w *Worker, checkOwner func() error) {
	if p == nil || w == nil || checkOwner == nil {
		return
	}
	release, err := p.acquireOutboundOperation(w)
	if err != nil {
		logger.Info("蜂窝 on_demand：设备已进入切换流程，跳过旧通话清理", "device", w.ID, "err", err)
		return
	}
	defer release()
	if err = checkOwner(); err != nil {
		logger.Info("蜂窝 on_demand：设备或 SIM 已更换，跳过旧通话清理", "device", w.ID, "err", err)
		return
	}
	if err = p.StopVoWiFiRuntimeForCellularIdle(w.ID); err != nil {
		logger.Warn("蜂窝 on_demand：挂断后停止 IMS 失败", "device", w.ID, "err", err)
	}
	if nc := w.NetworkController(); nc != nil && nc.IsConnected() {
		logger.Info("蜂窝 on_demand：挂断后关闭数据连接", "device", w.ID)
		if err = w.StopNetwork(); err != nil {
			logger.Warn("蜂窝 on_demand 关闭数据连接失败", "device", w.ID, "err", err)
		}
	}
}
