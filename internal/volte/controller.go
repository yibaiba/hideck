package volte

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
	"github.com/yibaiba/hideck/pkg/logger"
)

type Controller struct {
	host           Host
	provision      *Provisioner
	audio          *AudioRuntime
	media          mediaTable
	lteWait        time.Duration
	imsWait        time.Duration
	mu             sync.Mutex
	locks          map[string]*sync.Mutex
	sess           map[string]*session
	globalIncoming []func(voicehost.IncomingCall)
	globalEvents   []func(voicehost.CallEvent)
}

type session struct {
	events          sync.Mutex
	gen             uint64
	imsHooked       bool
	status          Status
	voice           *voiceSession
	alsaUnavailable bool
	lastVoiceAt     time.Time
	qpcmvTried      bool
	qpcmvOK         bool
}

const voiceUSBQuiet = 20 * time.Second

func (c *Controller) noteVoiceActivity(deviceID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	s := c.ensureLocked(strings.TrimSpace(deviceID))
	s.lastVoiceAt = time.Now()
	c.mu.Unlock()
}

func (c *Controller) VoiceUSBQuietRemaining(deviceID string) time.Duration {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.sess[strings.TrimSpace(deviceID)]
	if s == nil || s.lastVoiceAt.IsZero() {
		return 0
	}
	remain := voiceUSBQuiet - time.Since(s.lastVoiceAt)
	if remain < 0 {
		return 0
	}
	return remain
}

func NewController(host Host) *Controller {
	c := NewControllerWithBackup(host, DefaultBackupDir)
	c.lteWait = 45 * time.Second
	c.imsWait = 45 * time.Second
	return c
}

func NewControllerWithBackup(host Host, backupDir string) *Controller {
	c := &Controller{host: host, sess: make(map[string]*session), locks: make(map[string]*sync.Mutex)}
	c.provision = NewProvisioner(host, &FileStore{Dir: backupDir})
	return c
}

func (c *Controller) SetAudioRuntime(audio *AudioRuntime) {
	if c == nil {
		return
	}
	c.audio = audio
}

func (c *Controller) Enable(ctx context.Context, deviceID string) error {
	return c.EnableChecked(ctx, EnableRequest{DeviceID: deviceID})
}

// EnableRequest validates external ownership while holding the same device
// lock as Disable/DisableForHandoff, before installing a session or doing I/O.
type EnableRequest struct {
	DeviceID string
	Validate func() error
}

func (c *Controller) EnableChecked(ctx context.Context, request EnableRequest) error {
	deviceID := request.DeviceID
	deviceID = strings.TrimSpace(deviceID)
	if c == nil || c.host == nil || deviceID == "" {
		return errors.New("volte: controller is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return c.withDevice(deviceID, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if request.Validate != nil {
			if err := request.Validate(); err != nil {
				return err
			}
		}
		c.mu.Lock()
		s := c.sess[deviceID]
		if s == nil {
			s = &session{status: Status{DeviceID: deviceID, Phase: PhaseEnabling}}
			c.sess[deviceID] = s
		}
		s.gen++
		gen := s.gen
		s.status.Phase = PhaseEnabling
		s.status.LastError = ""
		c.mu.Unlock()

		if err := c.host.StopSoftwareIMS(deviceID); err != nil {
			return c.fail(deviceID, fmt.Errorf("stop software IMS: %w", err))
		}
		if _, err := execAT(c.host, deviceID, COPSNumericFormatCommand()); err != nil {
			logger.Debug("AT+COPS=3,2 失败，继续用当前 COPS 格式", "device", deviceID, "err", err)
		}
		if err := c.waitLTE(ctx, deviceID); err != nil {
			return c.fail(deviceID, err)
		}
		if err := c.provisionConfig(ctx, deviceID); err != nil {
			return err
		}
		if err := c.activateIMSPDN(deviceID); err != nil {
			logger.Warn("IMS PDN 未激活，继续等注册", "device", deviceID, "err", err)
			c.setError(deviceID, err)
		}
		c.patch(deviceID, func(st *Status) {
			if st.Phase == PhaseEnabling {
				st.Phase = PhaseRegistering
			}
		})
		c.attachIMS(deviceID, gen)
		c.attachVoice(deviceID)
		c.waitIMS(ctx, deviceID)
		return nil
	})
}

func (c *Controller) Restore(ctx context.Context, deviceID string) error {
	deviceID = strings.TrimSpace(deviceID)
	if c == nil || c.provision == nil || deviceID == "" {
		return errors.New("volte: controller is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	res, err := c.provision.Restore(ctx, deviceID)
	c.applyProvision(deviceID, res)
	if err != nil {
		return c.fail(deviceID, err)
	}
	c.Disable(deviceID)
	return nil
}

func (c *Controller) Disable(deviceID string) {
	deviceID = strings.TrimSpace(deviceID)
	if c == nil || deviceID == "" {
		return
	}
	_ = c.withDevice(deviceID, func() error {
		if active := c.ActiveCall(deviceID); active != nil {
			_ = c.HangupCall(context.Background(), deviceID, active.CallID)
			if m := c.media.take(active.CallID); m != nil {
				_ = m.Close()
			}
		}
		if c.host != nil {
			if err := c.host.ReleaseIMSClients(deviceID); err != nil {
				logger.Warn("释放原生 IMS 客户端失败", "device", deviceID, "err", err)
			}
		}
		c.mu.Lock()
		if s := c.sess[deviceID]; s != nil {
			s.gen++
		}
		delete(c.sess, deviceID)
		c.mu.Unlock()
		return nil
	})
}

func (c *Controller) Status(deviceID string) Status {
	if c == nil {
		return Status{DeviceID: deviceID, Phase: PhaseIdle}
	}
	deviceID = strings.TrimSpace(deviceID)
	c.mu.Lock()
	s := c.sess[deviceID]
	if s == nil {
		c.mu.Unlock()
		return Status{DeviceID: deviceID, Phase: PhaseIdle}
	}
	st := s.status
	qpcmvFailed := s.qpcmvTried && !s.qpcmvOK
	c.mu.Unlock()
	if c.usbAudioUnusable(deviceID) {
		st.UACUnusable = true
		st.UACEnabled = false
		st.AudioDevice = ""
		st.RebootRequired = false
		st.QPCMVFailed = false
		return st
	}
	audio := ""
	if c.host != nil {
		audio = strings.TrimSpace(c.host.AudioDevice(deviceID))
	}
	if audio == "" {
		return st
	}
	st.AudioDevice = audio
	st.UACEnabled = true
	st.RebootRequired = false
	st.QPCMVFailed = qpcmvFailed
	c.patch(deviceID, func(st *Status) {
		st.AudioDevice = audio
		st.UACEnabled = true
		st.RebootRequired = false
		st.QPCMVFailed = qpcmvFailed
	})
	return st
}

func (c *Controller) Active(deviceID string) bool {
	st := c.Status(deviceID)
	return st.Phase == PhaseRegistered || st.Phase == PhaseUnverified || st.Phase == PhaseEnabling || st.Phase == PhaseRegistering
}

func (c *Controller) provisionConfig(ctx context.Context, deviceID string) error {
	mcc, mnc, plmnErr := c.readPLMN(deviceID)
	if plmnErr != nil {
		return c.fail(deviceID, fmt.Errorf("read serving PLMN: %w", plmnErr))
	}
	c.patch(deviceID, func(st *Status) { st.PLMN = NormalizePLMN(mcc, mnc) })
	desired := Desired{
		IMSEnabled:   boolPtr(true),
		VoLTEEnabled: boolPtr(true),
		UACEnabled:   boolPtr(true),
		MCC:          mcc,
		MNC:          mnc,
	}
	res, err := c.provision.Ensure(ctx, deviceID, desired)
	c.applyProvision(deviceID, res)
	if err != nil && !res.Current.IMSEnabled && !errors.Is(err, ErrRebootRequired) && !errors.Is(err, ErrFieldDrift) {
		return c.fail(deviceID, err)
	}
	if err != nil {
		c.setError(deviceID, err)
	}
	if err := c.host.EnsureIMSClients(ctx, deviceID); err != nil {
		logger.Warn("QMI IMS/IMSA 客户端不可用", "device", deviceID, "err", err)
		c.patch(deviceID, func(st *Status) { st.QMIIMSUnavailable = true })
		return c.fail(deviceID, err)
	}
	if err := c.host.SetNativeIMS(ctx, deviceID, true); err != nil {
		logger.Warn("QMI 打开原生 IMS 失败，继续用 AT 结果", "device", deviceID, "err", err)
	}
	c.syncAudioStatus(deviceID, res.Current.UACEnabled)
	c.ensureVoicePCM(deviceID)
	if !res.Current.IMSEnabled {
		return c.fail(deviceID, fmt.Errorf("native IMS did not enable"))
	}
	return nil
}

func (c *Controller) applyProvision(deviceID string, res Result) {
	c.patch(deviceID, func(st *Status) {
		st.IMSEnabled = res.Current.IMSEnabled
		st.VoLTEEnabled = res.Current.VoLTEEnabled
		st.UACEnabled = false
		st.RebootRequired = res.RebootRequired || res.Current.UACEnabled
		st.ProvisionStage = res.Stage
		st.IMEITail = res.IMEITail
		if name := strings.TrimSpace(res.Current.MBNName); name != "" {
			st.MBNName = name
		} else if name := strings.TrimSpace(res.Target.MBNName); name != "" {
			st.MBNName = name
		}
	})
}

// ensureVoicePCM 在设备会话内只下发一次 QPCMV，用于 enable 阶段。
func (c *Controller) ensureVoicePCM(deviceID string) {
	c.applyVoicePCM(deviceID, false)
}

// armVoicePCM 在每次拨号前重新下发 QPCMV，必须在 VOICEDial 之前调用。
//
// 模组重启（USB 重新枚举）会把 USB 语音路由丢回内部音频通道，而本进程里的
// qpcmvTried 仍是 true，于是再也不补发：声卡能打开、下行正常，但播放侧每帧
// 写都 EPIPE（alsa_unrecovered 一路涨），对方完全听不到。切换一次 phone_mode
// 能"修好"，正是因为会话被销毁、闸门被复位。
//
// 时机很关键：必须在 VOICEDial 之前发。放到通话建立中间（callPCM 那一步）会
// 打断 QMI 语音状态机——实测通话停在 ringing，双方无声。
func (c *Controller) armVoicePCM(deviceID string) {
	c.applyVoicePCM(deviceID, true)
}

func (c *Controller) applyVoicePCM(deviceID string, force bool) {
	if c == nil || c.host == nil {
		return
	}
	deviceID = strings.TrimSpace(deviceID)
	c.mu.Lock()
	s := c.ensureLocked(deviceID)
	if s.qpcmvTried && !force {
		c.mu.Unlock()
		return
	}
	s.qpcmvTried = true
	c.mu.Unlock()
	if c.usbAudioUnusable(deviceID) || strings.TrimSpace(c.host.AudioDevice(deviceID)) == "" {
		c.mu.Lock()
		s = c.ensureLocked(deviceID)
		s.qpcmvOK = false
		s.alsaUnavailable = true
		s.status.UACUnusable = c.usbAudioUnusable(deviceID)
		s.status.QPCMVFailed = false
		c.mu.Unlock()
		if c.usbAudioUnusable(deviceID) {
			logger.Info("VoLTE 跳过不支持的模组声卡，继续无音频通话", "device", deviceID)
		}
		return
	}
	_, err := c.host.ExecuteAT(deviceID, "AT+QPCMV=1,2", 2*time.Second)
	c.mu.Lock()
	s = c.ensureLocked(deviceID)
	s.qpcmvOK = err == nil
	s.status.QPCMVFailed = err != nil
	c.mu.Unlock()
	if err != nil {
		// QPCMV 只是"把通话语音路由到 USB 声卡"的保险。命令失败（AT 通道被
		// 占用、超时）不代表声卡不可用，绝不能因此把整通电话降级成无音频：
		// 这里只保留状态位与告警，是否真的没声音交给端点激活和 XRUN 统计去
		// 暴露。此前这条路径会置 alsaUnavailable → callPCM 直接返回 nullPCM。
		logger.Warn("VoLTE 下发 QPCMV 失败，继续尝试打开声卡", "device", deviceID, "err", err)
		return
	}
	if force {
		// 模组重启后声卡可能已重新枚举：这次下发成功就让本会话重新尝试打开
		// 音频，不要被之前那次失败永久锁死。
		c.mu.Lock()
		s = c.ensureLocked(deviceID)
		s.alsaUnavailable = false
		c.mu.Unlock()
		logger.Info("VoLTE 拨号前重新下发 QPCMV", "device", deviceID)
	}
}

func (c *Controller) usbAudioUnusable(deviceID string) bool {
	if c == nil || c.host == nil {
		return false
	}
	u, ok := c.host.(interface{ USBAudioUnusable(string) bool })
	return ok && u.USBAudioUnusable(deviceID)
}

func (c *Controller) syncAudioStatus(deviceID string, nvUAC bool) {
	unusable := c.usbAudioUnusable(deviceID)
	audio := ""
	if !unusable && c.host != nil {
		audio = strings.TrimSpace(c.host.AudioDevice(deviceID))
	}
	c.patch(deviceID, func(st *Status) {
		st.UACUnusable = unusable
		st.AudioDevice = audio
		if unusable {
			st.UACEnabled = false
			st.RebootRequired = false
			return
		}
		if audio != "" {
			st.UACEnabled = true
			st.RebootRequired = false
			return
		}
		st.UACEnabled = false
		if nvUAC {
			st.RebootRequired = true
		}
	})
}

func (c *Controller) attachIMS(deviceID string, gen uint64) {
	if c == nil || c.host == nil {
		return
	}
	c.mu.Lock()
	s := c.sess[deviceID]
	if s == nil || s.imsHooked {
		c.mu.Unlock()
		return
	}
	s.imsHooked = true
	c.mu.Unlock()
	_ = c.host.OnIMSRegistration(deviceID, func(info *qmi.IMSARegistrationStatus) {
		s.events.Lock()
		defer s.events.Unlock()
		if !c.sessionGenerationLive(deviceID, s, gen) {
			return
		}
		c.patch(deviceID, func(st *Status) { applyIMSARegistration(st, info) })
	})
	_ = c.host.OnIMSServices(deviceID, func(info *qmi.IMSAServicesStatus) {
		s.events.Lock()
		defer s.events.Unlock()
		if !c.sessionGenerationLive(deviceID, s, gen) {
			return
		}
		c.patch(deviceID, func(st *Status) {
			if info != nil && info.HasVoiceServiceStatus {
				st.VoiceAvailable = info.VoiceServiceStatus == qmi.IMSAServiceAvailabilityAvailable
			}
		})
	})
}

func (c *Controller) sessionGenerationLive(deviceID string, owner *session, gen uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.sess[deviceID]
	return s != nil && s == owner && s.gen == gen
}

func (c *Controller) withDevice(deviceID string, fn func() error) error {
	c.mu.Lock()
	if c.locks == nil {
		c.locks = map[string]*sync.Mutex{}
	}
	lk := c.locks[deviceID]
	if lk == nil {
		lk = &sync.Mutex{}
		c.locks[deviceID] = lk
	}
	c.mu.Unlock()
	lk.Lock()
	defer lk.Unlock()
	return fn()
}

func applyIMSARegistration(st *Status, info *qmi.IMSARegistrationStatus) {
	if st == nil || info == nil || !info.HasStatus {
		return
	}
	switch info.Status {
	case qmi.IMSARegistrationStateRegistered, qmi.IMSARegistrationStateLimitedRegistered:
		st.IMSRegistered = true
		st.Phase = PhaseRegistered
		st.LastError = ""
	case qmi.IMSARegistrationStateRegistering:
		st.IMSRegistered = false
		st.Phase = PhaseRegistering
	default:
		st.IMSRegistered = false
		st.Phase = PhaseFailed
	}
}

func (c *Controller) refreshRegistration(ctx context.Context, deviceID string) {
	reg, err := c.host.IMSAStatus(ctx, deviceID)
	c.patch(deviceID, func(st *Status) {
		if err != nil {
			if st.IMSEnabled && st.VoLTEEnabled {
				st.Phase = PhaseUnverified
			}
			if st.LastError == "" {
				st.LastError = err.Error()
			}
			return
		}
		st.IMSRegistered = reg.Registered
		st.VoiceAvailable = reg.VoiceAvailable
		if reg.Registered {
			st.Phase = PhaseRegistered
			st.LastError = ""
		} else if st.IMSEnabled && st.VoLTEEnabled {
			st.Phase = PhaseUnverified
		}
	})
}

func (c *Controller) fail(deviceID string, err error) error {
	c.patch(deviceID, func(st *Status) {
		st.Phase = PhaseFailed
		st.LastError = err.Error()
	})
	return err
}

func (c *Controller) setError(deviceID string, err error) {
	if err == nil {
		return
	}
	c.patch(deviceID, func(st *Status) { st.LastError = err.Error() })
}

func (c *Controller) patch(deviceID string, fn func(*Status)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.sess[deviceID]
	if s == nil {
		s = &session{status: Status{DeviceID: deviceID}}
		c.sess[deviceID] = s
	}
	fn(&s.status)
}

func (c *Controller) readPLMN(deviceID string) (string, string, error) {
	resp, err := execAT(c.host, deviceID, COPSQueryCommand())
	if err != nil {
		return "", "", err
	}
	return ParseCOPS(resp)
}

func (c *Controller) waitLTE(ctx context.Context, deviceID string) error {
	check := func() error {
		resp, err := execAT(c.host, deviceID, CEREGQueryCommand())
		if err != nil {
			return fmt.Errorf("query CEREG: %w", err)
		}
		lte, err := ParseCEREG(resp)
		if err != nil {
			return err
		}
		c.patch(deviceID, func(st *Status) { st.LTERegistered = lte.Registered })
		if lte.Registered {
			return nil
		}
		return fmt.Errorf("LTE not registered (CEREG %d)", lte.Stat)
	}
	if err := check(); err == nil || c.lteWait <= 0 {
		return err
	}
	deadline := time.Now().Add(c.lteWait)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		time.Sleep(200 * time.Millisecond)
		if err := check(); err == nil {
			return nil
		}
	}
	return check()
}

func (c *Controller) activateIMSPDN(deviceID string) error {
	resp, err := execAT(c.host, deviceID, CGDCONTQueryCommand())
	if err != nil {
		return fmt.Errorf("query CGDCONT: %w", err)
	}
	ctxs, err := ParseCGDCONT(resp)
	if err != nil {
		return err
	}
	ims, ok := IMSContext(ctxs)
	if !ok {
		return fmt.Errorf("volte: no IMS APN in CGDCONT")
	}
	actResp, err := execAT(c.host, deviceID, CGACTQueryCommand())
	if err != nil {
		return fmt.Errorf("query CGACT: %w", err)
	}
	active, err := ParseCGACT(actResp)
	if err != nil {
		return err
	}
	if !active[ims.CID] {
		if _, err := execAT(c.host, deviceID, CGACTSetCommand(ims.CID, true)); err != nil {
			return fmt.Errorf("activate IMS PDN cid %d: %w", ims.CID, err)
		}
		actResp, err = execAT(c.host, deviceID, CGACTQueryCommand())
		if err != nil {
			return fmt.Errorf("re-query CGACT: %w", err)
		}
		active, err = ParseCGACT(actResp)
		if err != nil {
			return err
		}
		if !active[ims.CID] {
			return fmt.Errorf("IMS PDN cid %d still down", ims.CID)
		}
	}
	c.patch(deviceID, func(st *Status) { st.IMSPDNActive = true })
	return nil
}

func (c *Controller) waitIMS(ctx context.Context, deviceID string) {
	c.refreshRegistration(ctx, deviceID)
	if c.Status(deviceID).IMSRegistered || c.imsWait <= 0 {
		return
	}
	deadline := time.Now().Add(c.imsWait)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
		c.refreshRegistration(ctx, deviceID)
		if c.Status(deviceID).IMSRegistered {
			return
		}
	}
}
