package device

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
	"github.com/yibaiba/hideck/internal/volte"
	"github.com/yibaiba/hideck/pkg/logger"
)

func (p *Pool) NativeVoLTEController() *volte.Controller {
	if p == nil {
		return nil
	}
	return p.volteCtl
}

func (p *Pool) IsNativeVoLTE(deviceID string) bool {
	w := p.GetWorker(strings.TrimSpace(deviceID))
	if w == nil {
		return false
	}
	return IsNativeVoLTEMode(w.Config.PhoneMode) && PhoneServiceEnabled(w.Config)
}

func (p *Pool) NativeVoLTEStatus(deviceID string) volte.Status {
	if p == nil || p.volteCtl == nil {
		return volte.Status{DeviceID: deviceID, Phase: volte.PhaseIdle}
	}
	return p.volteCtl.Status(deviceID)
}

func (p *Pool) RestoreNativeVoLTE(ctx context.Context, deviceID string) error {
	if p == nil || p.volteCtl == nil {
		return fmt.Errorf("VoLTE 控制器未初始化")
	}
	return p.volteCtl.Restore(ctx, strings.TrimSpace(deviceID))
}

func (p *Pool) EnableNativeVoLTE(deviceID string) error {
	if err := p.CheckMBNIdle(deviceID); err != nil {
		return err
	}
	if p == nil || p.volteCtl == nil {
		return fmt.Errorf("VoLTE 控制器未初始化")
	}
	deviceID = strings.TrimSpace(deviceID)
	start := p.beginNativeVoLTESchedule(deviceID)
	if start == nil {
		return fmt.Errorf("设备 %s 正在启动 VoLTE 或切换模式", deviceID)
	}
	defer p.endNativeVoLTESchedule(deviceID, start)
	return p.enableNativeVoLTE(deviceID, start)
}

func (p *Pool) enableNativeVoLTE(deviceID string, start *nativeVoLTEStart) error {
	if err := start.waitTransition(); err != nil {
		return err
	}
	if err := p.validateNativeVoLTEStart(deviceID, start); err != nil {
		return err
	}
	class, err := ClassifyWorkerLebaraUKForControl(start.ctx, start.worker)
	if err != nil {
		return err
	}
	if class.BlocksVoWiFi() || class.IsLebara {
		return ErrLebaraUKRFLocked
	}
	if err := p.waitQMICoreReadyContext(start.ctx, deviceID, 30*time.Second); err != nil {
		if start.ctx.Err() != nil {
			return start.ctx.Err()
		}
		logger.Warn("VoLTE 等待 QMI 就绪失败，继续尝试 AT", "device", deviceID, "err", err)
	}
	release, err := p.AcquireDeviceOperation(deviceID)
	if err != nil {
		return err
	}
	defer release()
	if err := p.volteCtl.EnableChecked(start.ctx, volte.EnableRequest{DeviceID: deviceID,
		Validate: func() error { return p.validateNativeVoLTEStart(deviceID, start) },
	}); err != nil {
		return err
	}
	if err := p.validateNativeVoLTEStart(deviceID, start); err != nil {
		return err
	}
	// IMS PDN 起来后 qmi_wwan 可能把上网口拉起来。未开「网络」时主机不能走 3gnet。
	if w := p.GetWorker(deviceID); w != nil && !w.Config.NetworkEnabled {
		if err := p.applyNetworkPreference(w); err != nil {
			logger.Warn("VoLTE 已启用，抑制上网数据失败", "device", deviceID, "err", err)
		}
	}
	return nil
}

func (p *Pool) ScheduleNativeVoLTE(deviceID, reason string) {
	p.scheduleNativeVoLTE(deviceID, reason)
}

const nativeVoLTEStartAttempts = 3

func nativeVoLTERetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	return time.Duration(attempt) * 3 * time.Second
}

func isTransientVoLTEStartError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, volte.ErrNoUniqueProfile) ||
		errors.Is(err, volte.ErrVIDPIDMismatch) ||
		errors.Is(err, ErrLebaraUKRFLocked) {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "timed out") ||
		strings.Contains(msg, "busy") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection closed") ||
		strings.Contains(msg, "qmi-proxy") ||
		strings.Contains(msg, "service not ready") ||
		strings.Contains(msg, "服务未就绪")
}

func (p *Pool) scheduleNativeVoLTE(deviceID, reason string) {
	if p == nil || strings.TrimSpace(deviceID) == "" {
		return
	}
	deviceID = strings.TrimSpace(deviceID)
	start := p.beginNativeVoLTESchedule(deviceID)
	if start == nil {
		return
	}
	go func() {
		defer p.endNativeVoLTESchedule(deviceID, start)
		var err error
		for attempt := 1; attempt <= nativeVoLTEStartAttempts; attempt++ {
			err = p.enableNativeVoLTE(deviceID, start)
			if err == nil {
				return
			}
			if start.ctx.Err() != nil {
				return
			}
			if !isTransientVoLTEStartError(err) || attempt == nativeVoLTEStartAttempts {
				logger.Warn("启动原生 VoLTE 失败", "device", deviceID, "reason", reason, "attempt", attempt, "err", err)
				return
			}
			logger.Warn("启动原生 VoLTE 将重试", "device", deviceID, "reason", reason, "attempt", attempt, "err", err)
			timer := time.NewTimer(nativeVoLTERetryDelay(attempt))
			select {
			case <-start.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

func (p *Pool) stopNativeVoLTE(deviceID, reason string) {
	if p == nil || p.volteCtl == nil {
		return
	}
	p.cancelNativeVoLTEStart(deviceID)
	p.volteCtl.Disable(deviceID)
	logger.Debug("已停止原生 VoLTE 会话", "device", deviceID, "reason", reason)
}

func (p *Pool) StopSoftwareIMS(deviceID string) error {
	return p.stopSoftwareIMS(deviceID, true)
}

// stopSoftwareIMS with restoreRadio=false leaves the radio as it is. Card policy
// projection uses it: the teardown would otherwise bring the radio online from the
// not-yet-projected config, and a multi-IMSI SIM (DITO, Lebara UK) that sees a
// foreign network in that window switches to its roaming IMSI.
func (p *Pool) stopSoftwareIMS(deviceID string, restoreRadio bool) error {
	if p == nil {
		return nil
	}
	if !p.IsVoWiFiActive(deviceID) && !p.voWiFiHost().Starting(deviceID) {
		p.voWiFiHost().InvalidateRuntime(deviceID, "native_phone")
		return nil
	}
	return p.voWiFiHost().Disable(p.Context(), deviceID, "native_volte", restoreRadio)
}

func (p *Pool) SetNativeIMS(ctx context.Context, deviceID string, enabled bool) error {
	w := p.GetWorker(deviceID)
	if w == nil || w.QMICore == nil {
		return fmt.Errorf("设备 %s 没有 QMI", deviceID)
	}
	return w.QMICore.SetIMSServiceEnabled(ctx, enabled)
}

func (p *Pool) EnsureIMSClients(ctx context.Context, deviceID string) error {
	w := p.GetWorker(deviceID)
	if w == nil || w.QMICore == nil {
		return fmt.Errorf("设备 %s 没有 QMI", deviceID)
	}
	return w.QMICore.EnsureIMSClients(ctx)
}

func (p *Pool) ReleaseIMSClients(deviceID string) error {
	w := p.GetWorker(deviceID)
	if w == nil || w.QMICore == nil {
		return nil
	}
	return w.QMICore.ReleaseIMSClients()
}

func (p *Pool) OnIMSRegistration(deviceID string, handler func(*qmi.IMSARegistrationStatus)) error {
	w := p.GetWorker(deviceID)
	if w == nil || w.QMICore == nil {
		return fmt.Errorf("设备 %s 没有 QMI", deviceID)
	}
	return w.QMICore.OnIMSRegistrationStatus(handler)
}

func (p *Pool) OnIMSServices(deviceID string, handler func(*qmi.IMSAServicesStatus)) error {
	w := p.GetWorker(deviceID)
	if w == nil || w.QMICore == nil {
		return fmt.Errorf("设备 %s 没有 QMI", deviceID)
	}
	return w.QMICore.OnIMSServicesStatus(handler)
}

func (p *Pool) IMSAStatus(ctx context.Context, deviceID string) (volte.Registration, error) {
	w := p.GetWorker(deviceID)
	if w == nil || w.QMICore == nil {
		return volte.Registration{}, fmt.Errorf("设备 %s 没有 QMI", deviceID)
	}
	reg, err := w.QMICore.IMSAGetIMSRegistrationStatus(ctx)
	if err != nil {
		return volte.Registration{}, err
	}
	out := volte.Registration{Registered: reg != nil && reg.HasStatus &&
		(reg.Status == qmi.IMSARegistrationStateRegistered || reg.Status == qmi.IMSARegistrationStateLimitedRegistered)}
	svc, svcErr := w.QMICore.IMSAGetIMSServicesStatus(ctx)
	if svcErr == nil && svc != nil && svc.HasVoiceServiceStatus {
		out.VoiceAvailable = svc.VoiceServiceStatus == qmi.IMSAServiceAvailabilityAvailable
	}
	return out, nil
}

func (p *Pool) AudioDevice(deviceID string) string {
	w := p.GetWorker(deviceID)
	if w == nil {
		return ""
	}
	if p.USBAudioUnusable(deviceID) {
		w.Config.AudioDevice = ""
		return ""
	}
	if dev := strings.TrimSpace(w.Config.AudioDevice); dev != "" {
		return dev
	}
	usbPath := strings.TrimSpace(w.Config.USBPath)
	if usbPath == "" {
		return ""
	}
	dev, _ := findAudioDevice(usbPath)
	if dev != "" {
		w.Config.AudioDevice = dev
	}
	return dev
}

func (p *Pool) USBAudioUnusable(deviceID string) bool {
	w := p.GetWorker(deviceID)
	if w == nil {
		return false
	}
	return modemUACUnusable(w.Config.USBPath)
}

func (p *Pool) VOICEDial(ctx context.Context, deviceID, number string) (uint8, error) {
	release, err := p.AcquireDeviceOperation(deviceID)
	if err != nil {
		return 0, err
	}
	defer release()
	w := p.GetWorker(deviceID)
	if w == nil || w.QMICore == nil {
		return 0, fmt.Errorf("设备 %s 没有 QMI VOICE", deviceID)
	}
	return w.QMICore.VOICEDialCall(ctx, number)
}

func (p *Pool) VOICEAnswer(ctx context.Context, deviceID string, callID uint8) error {
	w := p.GetWorker(deviceID)
	if w == nil || w.QMICore == nil {
		return fmt.Errorf("设备 %s 没有 QMI VOICE", deviceID)
	}
	_, err := w.QMICore.VOICEAnswerCall(ctx, callID)
	return err
}

func (p *Pool) VOICEHangup(ctx context.Context, deviceID string, callID uint8) error {
	w := p.GetWorker(deviceID)
	if w == nil || w.QMICore == nil {
		return fmt.Errorf("设备 %s 没有 QMI VOICE", deviceID)
	}
	_, err := w.QMICore.VOICEEndCall(ctx, callID)
	return err
}

func (p *Pool) VOICEManageCalls(ctx context.Context, deviceID string, req qmi.VoiceManageCallsRequest) error {
	w := p.GetWorker(deviceID)
	if w == nil || w.QMICore == nil {
		return fmt.Errorf("设备 %s 没有 QMI VOICE", deviceID)
	}
	return w.QMICore.VOICEManageCalls(ctx, req)
}

func (p *Pool) VOICEBurstDTMF(ctx context.Context, deviceID string, callID uint8, digits string) error {
	w := p.GetWorker(deviceID)
	if w == nil || w.QMICore == nil {
		return fmt.Errorf("设备 %s 没有 QMI VOICE", deviceID)
	}
	_, err := w.QMICore.VOICEBurstDTMF(ctx, callID, digits)
	return err
}

func (p *Pool) BindVoiceDTMF(deviceID string) (volte.DTMFTransport, error) {
	w := p.GetWorker(deviceID)
	if w == nil || w.QMICore == nil {
		return nil, fmt.Errorf("设备 %s 没有 QMI VOICE", deviceID)
	}
	return w.QMICore, nil
}

func (p *Pool) VOICEStartContinuousDTMF(ctx context.Context, deviceID string, callID, digit uint8) error {
	w := p.GetWorker(deviceID)
	if w == nil || w.QMICore == nil {
		return fmt.Errorf("设备 %s 没有 QMI VOICE", deviceID)
	}
	_, err := w.QMICore.VOICEStartContinuousDTMF(ctx, callID, digit)
	return err
}

func (p *Pool) VOICEStopContinuousDTMF(ctx context.Context, deviceID string, callID uint8) error {
	w := p.GetWorker(deviceID)
	if w == nil || w.QMICore == nil {
		return fmt.Errorf("设备 %s 没有 QMI VOICE", deviceID)
	}
	_, err := w.QMICore.VOICEStopContinuousDTMF(ctx, callID)
	return err
}

func (p *Pool) OnVoiceStatus(deviceID string, handler func(*qmi.VoiceAllCallInfo)) error {
	w := p.GetWorker(deviceID)
	if w == nil || w.QMICore == nil {
		return fmt.Errorf("设备 %s 没有 QMI VOICE", deviceID)
	}
	return w.QMICore.OnVoiceCallStatus(handler)
}

func (p *Pool) VOICEGetAllCallInfo(ctx context.Context, deviceID string) (*qmi.VoiceAllCallInfo, error) {
	w := p.GetWorker(deviceID)
	if w == nil || w.QMICore == nil {
		return nil, fmt.Errorf("设备 %s 没有 QMI VOICE", deviceID)
	}
	return w.QMICore.VOICEGetAllCallInfo(ctx)
}
