package device

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/engine/ipsec"
	"github.com/iniwex5/vowifi-go/engine/swu"
	"github.com/iniwex5/vowifi-go/runtimehost"
	"github.com/iniwex5/vowifi-go/runtimehost/carrier"
	"github.com/iniwex5/vowifi-go/runtimehost/identity"
	"github.com/yibaiba/hideck/internal/backend"
	"github.com/yibaiba/hideck/internal/db"
	"github.com/yibaiba/hideck/internal/netprobe"
	innersim "github.com/yibaiba/hideck/internal/sim"
	"github.com/yibaiba/hideck/internal/upstreamproxy"
	"github.com/yibaiba/hideck/internal/vowifihost"
	"github.com/yibaiba/hideck/pkg/logger"
	"github.com/yibaiba/hideck/pkg/mbim"
)

type voWiFiStartContext struct {
	worker *Worker
	modem  runtimehost.Modem
	vowifihost.PreparedStart
	startedAt time.Time
}

type workerAKAProviderInput struct {
	worker   *Worker
	deviceID string
	modem    runtimehost.Modem
}

func (w workerAKAProviderInput) BackendMode() string {
	if w.worker == nil || w.worker.Backend == nil {
		return ""
	}
	return w.worker.Backend.Mode()
}

func (w workerAKAProviderInput) MBIMAKAProvider() (innersim.BackendAKAProvider, bool) {
	if w.worker == nil || w.worker.Backend == nil {
		return nil, false
	}
	provider, ok := w.worker.Backend.(interface {
		CalculateAKA(ctx context.Context, rand16, autn16 []byte) (res, ik, ck, auts []byte, err error)
	})
	if !ok || !strings.EqualFold(w.worker.Backend.Mode(), backend.BackendMBIM) {
		return nil, false
	}
	return provider, true
}

func (w workerAKAProviderInput) MBIMCapability() (*mbim.Capabilities, bool) {
	if w.worker == nil || w.worker.Backend == nil {
		return nil, false
	}
	cp, ok := w.worker.Backend.(interface{ Capability() *mbim.Capabilities })
	if !ok {
		return nil, false
	}
	c := cp.Capability()
	return c, c != nil
}

func (w workerAKAProviderInput) RuntimeModem() (innersim.ATModem, error) {
	modemIface := w.modem
	if modemIface == nil {
		var err error
		modemIface, err = BuildVoWiFiRuntimeModem(w.worker, w.deviceID)
		if err != nil {
			return nil, err
		}
	}
	modem, ok := modemIface.(innersim.ATModem)
	if !ok {
		return nil, fmt.Errorf("device %s runtime modem does not implement sim.ATModem", strings.TrimSpace(w.deviceID))
	}
	return modem, nil
}

func BuildAKAProvider(w *Worker, deviceID string) innersim.AKAProvider {
	return buildWorkerAKAProvider(w, deviceID, nil)
}

func buildWorkerAKAProvider(w *Worker, deviceID string, modem runtimehost.Modem) innersim.AKAProvider {
	if w != nil && strings.EqualFold(workerAKAProviderInput{worker: w}.BackendMode(), backend.BackendPCSC) {
		pcscBackend, ok := w.Backend.(*pcscDeviceBackend)
		if !ok {
			return nil
		}
		return &pcscAKAProvider{backend: pcscBackend, ICCID: w.CurrentICCID()}
	}
	return innersim.BuildAKAProvider(workerAKAProviderInput{worker: w, deviceID: deviceID, modem: modem})
}

func (p *Pool) Context() context.Context {
	if p == nil || p.ctx == nil {
		return context.Background()
	}
	return p.ctx
}

func (p *Pool) PrepareStart(deviceID, traceID, runtimeEPDGOverride string) (vowifihost.PreparedStart, error) {
	startCtx, err := p.prepareVoWiFiStartContext(deviceID, traceID, runtimeEPDGOverride)
	if err != nil {
		return vowifihost.PreparedStart{}, err
	}
	prepared := startCtx.PreparedStart
	prepared.Modem = startCtx.modem
	return prepared, nil
}

func (p *Pool) BeforeStart(deviceID string, modemIface runtimehost.Modem, proxyCfg *runtimehost.ProxyConfig) func(context.Context, runtimehost.SessionConfig) error {
	return p.beforeVoWiFiStart(deviceID, modemIface, proxyCfg)
}

func (p *Pool) HandleStartupError(req vowifihost.StartupErrorRequest) error {
	return p.handleVoWiFiStartupError(req.TraceID, req.DeviceID, req.RuntimeEPDGOverride, req.Generation, req.StartedAt, p.GetWorker(req.DeviceID), req.State, req.Err)
}

func (p *Pool) MarkRuntimeStarted(req vowifihost.RuntimeStartedRequest) {
	w := p.GetWorker(req.DeviceID)
	if w == nil {
		return
	}
	if w.Config.PhoneMode == "cellular" {
		w.setCellularRadioSuppressed(false)
	} else {
		w.setCellularRadioSuppressed(true)
	}
	w.smsMode = smsModeVoWiFi
	if w.Modem != nil {
		w.Modem.SetNewSMSHandler(nil)
		w.Modem.SetSMSCallback(nil)
		w.Modem.SetSMSProcessor(nil)
		w.Modem.SetSMSReadinessCheck(nil)
		w.Modem.SetDisableURCRead(true)
	}
}

func (p *Pool) prepareVoWiFiStartContext(deviceID, traceID, runtimeEPDGOverride string) (voWiFiStartContext, error) {
	startCtx := voWiFiStartContext{startedAt: time.Now()}

	w := p.GetWorker(deviceID)
	if w == nil {
		return startCtx, fmt.Errorf("设备 %s 不存在", deviceID)
	}
	startCtx.worker = w
	w.restoreNetworkAfterVoWiFi = w.Config.NetworkEnabled

	if w.Config.PhoneMode == "cellular" {
		class, err := ClassifyWorkerLebaraUKForControl(p.Context(), w)
		if err != nil {
			return startCtx, fmt.Errorf("识别 Lebara UK 蜂窝模式策略失败: %w", err)
		}
		if class.IsLebara {
			p.enforceLebaraUKRadioOff(w, "cellular_start_blocked")
			return startCtx, ErrLebaraUKRFLocked
		}
		return p.prepareCellularStartContext(startCtx, w, deviceID, traceID, runtimeEPDGOverride)
	}

	if err := w.suppressCellularRegistration(p.Context(), "vowifi_start"); err != nil {
		return startCtx, fmt.Errorf("VoWiFi 启动前停止蜂窝驻网协调失败: %w", err)
	}

	modemIface, errModemIface := newVoWiFiModemInterface(w, deviceID)
	if errModemIface != nil {
		return startCtx, errModemIface
	}
	startCtx.modem = modemIface
	if _, ok := modemIface.(*qmiModemAdapter); ok {
		logger.Info("VoWiFi 使用 QMI 模式鉴权", "trace_id", traceID, "device", deviceID)
	}

	w.cacheMu.RLock()
	identityReady := w.state.Identity.Ready
	w.cacheMu.RUnlock()
	if !identityReady {
		if err := w.RefreshIdentityLive(nil, "enable_vowifi"); err != nil {
			logger.Error("VoWiFi 启动前刷新当前设备身份失败",
				"trace_id", traceID,
				"device", deviceID,
				"err", err)
			return startCtx, err
		}
		p.PersistIdentityState(w)
	}

	currentStatus := w.ProjectDeviceStatus()
	logger.Info("VoWiFi 启动前读取当前设备身份",
		"trace_id", traceID,
		"device", deviceID,
		"iccid", strings.TrimSpace(currentStatus.ICCID),
		"imsi", strings.TrimSpace(currentStatus.IMSI),
		"imei", strings.TrimSpace(currentStatus.IMEI))

	startProfile, errProfile := p.buildVoWiFiStartProfile(w, traceID)
	if errProfile != nil {
		logger.Error("构建 VoWiFi 启动画像失败", "trace_id", traceID, "device", deviceID, "err", errProfile)
		return startCtx, errProfile
	}
	startCtx.Profile = startProfile

	akaProvider := buildWorkerAKAProvider(w, deviceID, modemIface)
	if akaProvider == nil {
		if strings.EqualFold(workerAKAProviderInput{worker: w}.BackendMode(), backend.BackendMBIM) {
			return startCtx, fmt.Errorf("设备 %s 的 MBIM 不支持 AKA(AUTH 与逻辑通道均不可用),如需 VoWiFi 请切 QMI 组态", deviceID)
		}
		return startCtx, fmt.Errorf("设备 %s 无可用 AKA provider", deviceID)
	}
	if strings.EqualFold(workerAKAProviderInput{worker: w}.BackendMode(), backend.BackendMBIM) {
		logger.Info("VoWiFi 使用 MBIM Auth(AKA) 鉴权", "trace_id", traceID, "device", deviceID)
	} else {
		logger.Info("VoWiFi 使用 APDU(AKA) 鉴权", "trace_id", traceID, "device", deviceID)
	}
	if carrier.IsVoWiFiBlockedMCC(startProfile.MCC) {
		err := carrier.NewVoWiFiBlockedMCCError(startProfile.MCC)
		logger.Warn("VoWiFi 启动被运营商策略拦截",
			"trace_id", traceID,
			"device", deviceID,
			"mcc", formatVoWiFiPLMN3(startProfile.MCC),
			"imsi", startProfile.IMSI,
			"err", err)
		logVoWiFiFailureSummary(traceID, deviceID, "startup", "policy", err.Error(), false, 0)
		return startCtx, err
	}
	class, classifyErr := ClassifyWorkerLebaraUK(w)
	if classifyErr != nil {
		err := fmt.Errorf("识别 Lebara UK VoWiFi 策略失败: %w", classifyErr)
		logVoWiFiFailureSummary(traceID, deviceID, "startup", "policy", err.Error(), false, 0)
		return startCtx, err
	}
	if p.lebaraUKRecoverBlocksVoWiFi(deviceID) {
		err := fmt.Errorf("正在恢复 Lebara 英国身份，暂不启动 VoWiFi")
		logger.Warn("VoWiFi 启动被 Lebara 清污维护锁拦截",
			"trace_id", traceID,
			"device", deviceID,
			"err", err)
		logVoWiFiFailureSummary(traceID, deviceID, "startup", "policy", err.Error(), false, 0)
		return startCtx, err
	}
	if class.BlocksVoWiFi() {
		err := NewLebaraUKFlippedIMSIError(class.LiveIMSI)
		logger.Warn("VoWiFi 启动被 Lebara UK 切卡拦截",
			"trace_id", traceID,
			"device", deviceID,
			"imsi", class.LiveIMSI,
			"err", err)
		logVoWiFiFailureSummary(traceID, deviceID, "startup", "policy", err.Error(), false, 0)
		return startCtx, err
	}

	runtimehost.SetLogger(slog.Default())
	prepared, errPrepare := identity.PrepareStart(identity.PrepareStartInput{
		DeviceID:            deviceID,
		Profile:             startProfile,
		RuntimeEPDGOverride: runtimeEPDGOverride,
		Access:              runtimehost.NewModemAccessAdapter(modemIface),
	})
	if errPrepare != nil {
		logger.Warn("VoWiFi 启动画像准备失败",
			"trace_id", traceID,
			"device", deviceID,
			"err", errPrepare)
		logVoWiFiFailureSummary(traceID, deviceID, "startup", "identity", errPrepare.Error(), false, 0)
		return startCtx, errPrepare
	}
	startCtx.Prepared = prepared
	startCtx.Mode = runtimehost.StartModeMain
	if strings.EqualFold(w.Backend.Mode(), backend.BackendPCSC) {
		startCtx.Mode = runtimehost.StartModeReader
	}
	if preferred, ok := akaProvider.(innersim.AKAWithPreferenceProvider); ok {
		akaProvider = innersim.WrapPreferredAKAProvider(preferred, string(prepared.IMSIdentity.AKAAppPreference))
	}
	startCtx.SIM = runtimehost.NewReaderSIMAdapter(akaProvider)
	logger.Info("VoWiFi 启动画像已准备",
		"trace_id", traceID,
		"device", deviceID,
		"matched_plmn", prepared.EffectiveCarrier.MCC+"/"+prepared.EffectiveCarrier.MNC,
		"preset_id", prepared.EffectiveCarrier.PresetID,
		"device_model", prepared.CarrierConfig.DeviceModel,
		"ike_proposals", prepared.CarrierConfig.IKEProposals,
		"esp_proposals", prepared.CarrierConfig.ESPProposals,
		"reauth_seconds", prepared.CarrierConfig.ReauthIntervalSeconds,
		"epdg_source", prepared.EPDGSource,
		"epdg", prepared.EPDGAddr,
		"identity_source", prepared.IdentityIMEISource,
		"requested_source", prepared.IMSIdentity.RequestedSource,
		"actual_source", prepared.IMSIdentity.ActualSource,
		"aka_app_preference", prepared.IMSIdentity.AKAAppPreference,
		"applied", prepared.IMSIdentity.Applied)

	proxy, errProxy := resolveVoWiFiCountryProxy(voWiFiProxyResolveRequest{
		Ctx:      p.Context(),
		HomeMCC:  startProfile.MCC,
		TraceID:  traceID,
		DeviceID: deviceID,
		ICCID:    w.CurrentICCID(),
	})
	if errProxy != nil {
		return startCtx, errProxy
	}
	startCtx.Proxy = proxy

	if nc := w.NetworkController(); nc != nil {
		logger.Info("VoWiFi 启用中，停止网络功能", "trace_id", traceID, "device", deviceID)
		if err := w.StopNetwork(); err != nil {
			logger.Warn("断开数据连接失败，继续启动 VoWiFi", "trace_id", traceID, "device", deviceID, "err", err)
		}
	}

	if err := enterVoWiFiRFOff(p.Context(), w, traceID); err != nil {
		return startCtx, err
	}

	startCtx.NetworkMode = modemIface.GetNetworkMode()
	startCtx.StartupState = newVoWiFiSIMReadyStartupState(deviceID, swu.DataplaneModeUserspace, startCtx.NetworkMode, time.Now())
	p.recordVoWiFiStartupState(deviceID, startCtx.StartupState)
	return startCtx, nil
}

// prepareCellularStartContext prepares the VoWiFi start context for cellular
// mode: keeps radio on, keeps data connected (always) or defers (on_demand),
// suppresses native IMS via QMI, and sets a TunnelFactory that binds the SWu
// tunnel socket to the cellular interface via SO_BINDTODEVICE.
func (p *Pool) prepareCellularStartContext(
	startCtx voWiFiStartContext,
	w *Worker,
	deviceID, traceID, runtimeEPDGOverride string,
) (voWiFiStartContext, error) {
	// always: data must be up before SWu. on_demand: do not start without a bearer
	// (EnableVoWiFi and desired-reconcile skip tunnel setup until dial time).
	if w.Config.DataStrategy != "always" {
		if nc := w.NetworkController(); nc == nil || !nc.IsConnected() {
			return startCtx, errCellularOnDemandIdle
		}
	} else if err := p.EnsureCellularData(p.Context(), deviceID); err != nil {
		return startCtx, err
	}

	// Suppress native IMS so the software IMS stack can take over.
	// Some Qualcomm SKUs have no QMI IMS service; that is expected, not a start loop.
	if w.QMICore != nil {
		if err := w.QMICore.SetIMSServiceEnabled(p.Context(), false); err != nil {
			if isQMIServiceUnsupported(err) {
				logger.Debug("蜂窝模式跳过原生 IMS 抑制：硬件不支持 QMI IMS", "trace_id", traceID, "device", deviceID, "err", err)
			} else {
				logger.Warn("蜂窝模式抑制原生 IMS 失败，继续启动", "trace_id", traceID, "device", deviceID, "err", err)
			}
		} else {
			logger.Info("蜂窝模式已抑制原生 IMS", "trace_id", traceID, "device", deviceID)
		}
	}

	ifaceName := strings.TrimSpace(w.Config.Interface)
	hasInternet := false
	if ifaceName != "" {
		probeCtx, cancel := context.WithTimeout(p.Context(), 3*time.Second)
		hasInternet = netprobe.HasDirectIPConnectivity(probeCtx, ifaceName, 3*time.Second)
		cancel()
	}
	if hasInternet {
		logger.Info("蜂窝模式外网探测成功，SWu 绑定蜂窝接口", "trace_id", traceID, "device", deviceID, "interface", ifaceName)
	} else {
		logger.Warn("蜂窝模式外网探测失败", "trace_id", traceID, "device", deviceID, "interface", ifaceName)
	}

	// Reuse the common identity/modem preparation from the WiFi calling path.
	// We skip suppressCellularRegistration, StopNetwork, and enterVoWiFiRFOff.

	modemIface, errModemIface := newVoWiFiModemInterface(w, deviceID)
	if errModemIface != nil {
		return startCtx, errModemIface
	}
	startCtx.modem = modemIface

	w.cacheMu.RLock()
	identityReady := w.state.Identity.Ready
	w.cacheMu.RUnlock()
	if !identityReady {
		if err := w.RefreshIdentityLive(nil, "enable_cellular"); err != nil {
			logger.Error("蜂窝模式启动前刷新设备身份失败", "trace_id", traceID, "device", deviceID, "err", err)
			return startCtx, err
		}
		p.PersistIdentityState(w)
	}

	startProfile, errProfile := p.buildVoWiFiStartProfile(w, traceID)
	if errProfile != nil {
		logger.Error("蜂窝模式构建启动画像失败", "trace_id", traceID, "device", deviceID, "err", errProfile)
		return startCtx, errProfile
	}
	startCtx.Profile = startProfile

	akaProvider := buildWorkerAKAProvider(w, deviceID, modemIface)
	if akaProvider == nil {
		return startCtx, fmt.Errorf("设备 %s 无可用 AKA provider", deviceID)
	}

	runtimehost.SetLogger(slog.Default())
	prepared, errPrepare := identity.PrepareStart(identity.PrepareStartInput{
		DeviceID:            deviceID,
		Profile:             startProfile,
		RuntimeEPDGOverride: runtimeEPDGOverride,
		Access:              runtimehost.NewModemAccessAdapter(modemIface),
	})
	if errPrepare != nil {
		logger.Warn("蜂窝模式启动画像准备失败", "trace_id", traceID, "device", deviceID, "err", errPrepare)
		return startCtx, errPrepare
	}
	startCtx.Prepared = prepared
	startCtx.Mode = runtimehost.StartModeMain
	if strings.EqualFold(w.Backend.Mode(), backend.BackendPCSC) {
		startCtx.Mode = runtimehost.StartModeReader
	}
	if preferred, ok := akaProvider.(innersim.AKAWithPreferenceProvider); ok {
		akaProvider = innersim.WrapPreferredAKAProvider(preferred, string(prepared.IMSIdentity.AKAAppPreference))
	}
	startCtx.SIM = runtimehost.NewReaderSIMAdapter(akaProvider)

	countryProxy, errProxy := resolveCellularIMSCountryProxy(hasInternet, ifaceName, voWiFiProxyResolveRequest{
		Ctx:      p.Context(),
		HomeMCC:  startProfile.MCC,
		TraceID:  traceID,
		DeviceID: deviceID,
		ICCID:    w.CurrentICCID(),
	})
	if errProxy != nil {
		return startCtx, errProxy
	}
	transport, errTransport := selectCellularIMSTransport(hasInternet, ifaceName, countryProxy)
	if errTransport != nil {
		return startCtx, errTransport
	}
	if transport.ViaProxy {
		startCtx.Proxy = transport.Proxy
		startCtx.TunnelFactory = nil
		logger.Warn("蜂窝数据无外网，改走国家前置代理建立 IMS",
			"trace_id", traceID,
			"device", deviceID,
			"interface", ifaceName,
			"proxy_id", transport.Proxy.ID,
			"proxy_addr", transport.Proxy.Addr)
		if w.Config.DataStrategy != "always" {
			if nc := w.NetworkController(); nc != nil && nc.IsConnected() {
				if err := w.StopNetwork(); err != nil {
					logger.Warn("蜂窝 on_demand 回退代理后关闭数据失败", "trace_id", traceID, "device", deviceID, "err", err)
				} else {
					logger.Info("蜂窝 on_demand 已关闭无外网数据连接", "trace_id", traceID, "device", deviceID)
				}
			}
		}
	} else if transport.BindInterface != "" {
		startCtx.Proxy = nil
		startCtx.TunnelFactory = buildCellularTunnelFactory(deviceID, transport.BindInterface)
		logger.Info("蜂窝模式隧道绑定接口", "trace_id", traceID, "device", deviceID, "interface", transport.BindInterface)
	} else {
		startCtx.Proxy = countryProxy
	}
	startCtx.NetworkMode = modemIface.GetNetworkMode()
	startCtx.StartupState = newVoWiFiSIMReadyStartupState(deviceID, swu.DataplaneModeUserspace, startCtx.NetworkMode, time.Now())
	p.recordVoWiFiStartupState(deviceID, startCtx.StartupState)
	return startCtx, nil
}

// buildCellularTunnelFactory returns a TunnelFactory that wraps the default
// SWu tunnel adapter after injecting a TransportFactory that binds the UDP
// socket to a specific network interface (SO_BINDTODEVICE).
func isQMIServiceUnsupported(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not supported")
}

func buildCellularTunnelFactory(deviceID, interfaceName string) runtimehost.TunnelFactory {
	return func(cfg *swu.Config) (runtimehost.Tunnel, error) {
		if cfg.TransportFactory == nil {
			cfg.TransportFactory = func(local, remote string) (ipsec.Transport, error) {
				return ipsec.NewSocketManagerWithOptions(deviceID, local, remote, "", ipsec.SocketOptions{
					BindToDevice: interfaceName,
				})
			}
		}
		return runtimehost.NewDefaultTunnel(deviceID, cfg)
	}
}

func enterVoWiFiRFOff(ctx context.Context, w *Worker, traceID string) error {
	if w == nil || w.Backend == nil {
		return fmt.Errorf("VoWiFi 启动缺少射频控制后端")
	}
	if strings.EqualFold(w.Backend.Mode(), backend.BackendPCSC) {
		logger.Info("PC/SC 读卡器没有蜂窝射频，跳过射频切换", "trace_id", traceID, "device", w.ID)
		return nil
	}
	if strings.EqualFold(w.Backend.Mode(), backend.BackendMBIM) {
		logger.Info("MBIM 后端不支持真正的低功耗模式", "trace_id", traceID, "device", w.ID)
		return nil
	}
	mode, err := w.Backend.GetOperatingMode(ctx)
	if err != nil {
		return fmt.Errorf("VoWiFi 启动前读取射频模式失败: %w", err)
	}
	if isPersistFlightOperatingMode(mode) {
		logger.Info("设备已处于飞行模式，跳过冗余的飞行模式切换",
			"trace_id", traceID, "device", w.ID, "backend", w.Backend.Mode())
		return nil
	}
	logger.Info("进入飞行模式以禁用原生 IMS 注册",
		"trace_id", traceID, "device", w.ID, "backend", w.Backend.Mode())
	if err := w.Backend.SetOperatingMode(ctx, backend.ModeRFOff); err != nil {
		return fmt.Errorf("进入飞行模式失败: %w", err)
	}
	if err := sleepQMIRegistrationPoll(ctx, 500*time.Millisecond); err != nil {
		return fmt.Errorf("等待飞行模式生效失败: %w", err)
	}
	mode, err = w.Backend.GetOperatingMode(ctx)
	if err != nil {
		return fmt.Errorf("验证飞行模式失败: %w", err)
	}
	if !isFlightOperatingMode(mode) {
		return fmt.Errorf("飞行模式未生效: mode=%d", int(mode))
	}
	return nil
}

type voWiFiProxyResolveRequest struct {
	Ctx      context.Context
	HomeMCC  string
	TraceID  string
	DeviceID string
	ICCID    string
}

func resolveVoWiFiCountryProxy(req voWiFiProxyResolveRequest) (*runtimehost.ProxyConfig, error) {
	cfg, matched, err := resolveCardVoWiFiUpstreamProxy(req.ICCID, req.TraceID, req.DeviceID)
	if err != nil {
		return nil, err
	}
	if matched {
		return cfg, nil
	}
	proxies, countryCode, err := db.GetHomeMCCUpstreamProxies(req.HomeMCC)
	if err != nil {
		return nil, fmt.Errorf("读取 VoWiFi 国家前置代理配置失败: %w", err)
	}
	pick := pickCountryPoolProxy(req.Ctx, proxies)
	if pick.Proxy == nil {
		logger.Info("VoWiFi 国家前置代理未命中，使用直连",
			"trace_id", req.TraceID,
			"device", req.DeviceID,
			"home_mcc", strings.TrimSpace(req.HomeMCC),
			"proxy_country_code", countryCode,
			"mcc_table_ready", upstreamproxy.CountryTableReady(),
			"proxy_route", "direct")
		return nil, nil
	}
	route := "country_rule"
	if len(proxies) > 1 {
		route = "country_pool"
	}
	logger.Info("VoWiFi 国家前置代理已命中",
		"trace_id", req.TraceID,
		"device", req.DeviceID,
		"home_mcc", strings.TrimSpace(req.HomeMCC),
		"proxy_country_code", countryCode,
		"upstream_proxy_id", pick.Proxy.ID,
		"proxy_pool_size", len(proxies),
		"proxy_route", route,
		"proxy_pick_tier", pick.Tier,
		"skipped_proxies", formatCountryPoolSkips(pick.Skipped))
	return proxyConfigFromDB(pick.Proxy), nil
}

func resolveCellularIMSCountryProxy(hasInternet bool, iface string, req voWiFiProxyResolveRequest) (*runtimehost.ProxyConfig, error) {
	if cellularIMSUsesBoundInterface(hasInternet, iface) {
		return nil, nil
	}
	return resolveVoWiFiCountryProxy(req)
}

func resolveCardVoWiFiUpstreamProxy(iccid, traceID, deviceID string) (*runtimehost.ProxyConfig, bool, error) {
	iccid = db.CanonicalICCID(iccid)
	if iccid == "" {
		return nil, false, nil
	}
	pol, err := db.GetCardPolicy(iccid)
	if err != nil {
		if errors.Is(err, db.ErrCardPolicyNotFound) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("读取 VoWiFi 卡级前置代理策略失败: %w", err)
	}
	id := db.NormalizeVoWiFiUpstreamProxyID(pol.VowifiUpstreamProxyID)
	if id == "" {
		return nil, false, nil
	}
	if id == db.VoWiFiUpstreamProxyDirect {
		logger.Info("VoWiFi 卡策略指定直连",
			"trace_id", traceID,
			"device", deviceID,
			"iccid", iccid,
			"proxy_route", "card_direct")
		return nil, true, nil
	}
	proxy, err := db.GetUpstreamProxyByID(id)
	if err != nil {
		return nil, true, fmt.Errorf("读取卡级固定前置代理 %s 失败: %w", id, err)
	}
	if proxy == nil {
		return nil, true, fmt.Errorf("卡级固定前置代理 %s 不存在", id)
	}
	if !proxy.Enabled {
		return nil, true, fmt.Errorf("卡级固定前置代理 %s 已禁用", id)
	}
	logger.Info("VoWiFi 卡策略指定前置代理",
		"trace_id", traceID,
		"device", deviceID,
		"iccid", iccid,
		"upstream_proxy_id", proxy.ID,
		"proxy_route", "card_override")
	return proxyConfigFromDB(proxy), true, nil
}

func proxyConfigFromDB(proxy *db.UpstreamProxy) *runtimehost.ProxyConfig {
	if proxy == nil {
		return nil
	}
	return &runtimehost.ProxyConfig{
		ID:       proxy.ID,
		Addr:     proxy.Addr,
		Username: proxy.Username,
		Password: proxy.Password,
		Enabled:  proxy.Enabled,
	}
}

const (
	countryPoolPickTimeout = 2 * time.Second
	countryPoolPickGrace   = 400 * time.Millisecond

	countryPoolTierUDP       = "udp"
	countryPoolTierAssociate = "associate"
	countryPoolTierAny       = "any"
	countryPoolTierSingle    = "single"
)

type socks5ProxyProber func(context.Context, db.UpstreamProxy) (upstreamproxy.ProbeResult, error)

type countryPoolSkip struct {
	ID     string
	Stage  string
	Reason string
}

type countryPoolPickResult struct {
	Proxy   *db.UpstreamProxy
	Tier    string
	Skipped []countryPoolSkip
}

type countryPoolProbeOutcome struct {
	proxy db.UpstreamProxy
	res   upstreamproxy.ProbeResult
}

func probeSOCKS5ProxyForPick(ctx context.Context, proxy db.UpstreamProxy) (upstreamproxy.ProbeResult, error) {
	return upstreamproxy.ProbeSOCKS5(ctx, upstreamproxy.ProbeConfig{
		ProxyAddr: proxy.Addr,
		Username:  proxy.Username,
		Password:  proxy.Password,
		Timeout:   countryPoolPickTimeout,
	})
}

func pickCountryPoolProxy(ctx context.Context, proxies []db.UpstreamProxy) countryPoolPickResult {
	return pickCountryPoolProxyWith(ctx, proxies, probeSOCKS5ProxyForPick)
}

func pickCountryPoolProxyWith(ctx context.Context, proxies []db.UpstreamProxy, probe socks5ProxyProber) countryPoolPickResult {
	if len(proxies) == 0 {
		return countryPoolPickResult{}
	}
	if len(proxies) == 1 || probe == nil {
		return countryPoolPickResult{Proxy: db.PickUpstreamProxy(proxies), Tier: countryPoolTierSingle}
	}
	parent := ctx
	if parent == nil {
		parent = context.Background()
	}
	pickCtx, cancel := context.WithTimeout(parent, countryPoolPickTimeout)
	defer cancel()

	ch := make(chan countryPoolProbeOutcome, len(proxies))
	for _, proxy := range proxies {
		proxy := proxy
		go func() {
			res, _ := probe(pickCtx, proxy)
			ch <- countryPoolProbeOutcome{proxy: proxy, res: res}
		}()
	}

	collected := make([]countryPoolProbeOutcome, 0, len(proxies))
	pending := len(proxies)
	var grace *time.Timer
	stopGrace := func() {
		if grace != nil {
			grace.Stop()
		}
	}
	defer stopGrace()

	for pending > 0 {
		var graceC <-chan time.Time
		if grace != nil {
			graceC = grace.C
		}
		select {
		case item := <-ch:
			collected = append(collected, item)
			pending--
			if item.res.OK() && grace == nil {
				grace = time.NewTimer(countryPoolPickGrace)
			}
		case <-graceC:
			cancel()
			collected = append(collected, drainCountryPoolOutcomes(ch)...)
			pending = 0
		case <-pickCtx.Done():
			collected = append(collected, drainCountryPoolOutcomes(ch)...)
			pending = 0
		}
	}
	return classifyCountryPoolPick(proxies, collected)
}

func drainCountryPoolOutcomes(ch <-chan countryPoolProbeOutcome) []countryPoolProbeOutcome {
	out := make([]countryPoolProbeOutcome, 0)
	for {
		select {
		case item := <-ch:
			out = append(out, item)
		default:
			return out
		}
	}
}

func classifyCountryPoolPick(proxies []db.UpstreamProxy, collected []countryPoolProbeOutcome) countryPoolPickResult {
	byID := make(map[string]countryPoolProbeOutcome, len(collected))
	for _, item := range collected {
		byID[item.proxy.ID] = item
	}
	udpOK := make([]db.UpstreamProxy, 0, len(proxies))
	assocOK := make([]db.UpstreamProxy, 0, len(proxies))
	skipped := make([]countryPoolSkip, 0)
	for _, proxy := range proxies {
		item, ok := byID[proxy.ID]
		if !ok {
			skipped = append(skipped, countryPoolSkip{
				ID: proxy.ID, Stage: "cancelled", Reason: "已有 UDP 正常节点，停止等待",
			})
			continue
		}
		switch {
		case item.res.OK():
			udpOK = append(udpOK, proxy)
		case item.res.UDPAssociationOK():
			assocOK = append(assocOK, proxy)
			skipped = append(skipped, countryPoolSkip{
				ID: proxy.ID, Stage: item.res.Stage, Reason: item.res.FailureSummary(),
			})
		default:
			skipped = append(skipped, countryPoolSkip{
				ID: proxy.ID, Stage: item.res.Stage, Reason: item.res.FailureSummary(),
			})
		}
	}
	result := countryPoolPickResult{Skipped: skipped}
	switch {
	case len(udpOK) > 0:
		result.Proxy = db.PickUpstreamProxy(udpOK)
		result.Tier = countryPoolTierUDP
	case len(assocOK) > 0:
		result.Proxy = db.PickUpstreamProxy(assocOK)
		result.Tier = countryPoolTierAssociate
	default:
		result.Proxy = db.PickUpstreamProxy(proxies)
		result.Tier = countryPoolTierAny
	}
	return result
}

func formatCountryPoolSkips(skips []countryPoolSkip) []string {
	out := make([]string, 0, len(skips))
	for _, skip := range skips {
		part := skip.ID
		if skip.Stage != "" {
			part += ":" + skip.Stage
		}
		if skip.Reason != "" {
			part += ":" + skip.Reason
		}
		out = append(out, part)
	}
	return out
}

func (p *Pool) beforeVoWiFiStart(deviceID string, modemIface runtimehost.Modem, proxyCfg *runtimehost.ProxyConfig) func(context.Context, runtimehost.SessionConfig) error {
	return func(startCtx context.Context, cfg runtimehost.SessionConfig) error {
		startupState := newVoWiFiSIMReadyStartupState(deviceID, cfg.DataplaneMode, modemIface.GetNetworkMode(), time.Now())
		startupState.RegStatus, startupState.RegStatusText = modemIface.GetRegStatus()
		p.recordVoWiFiStartupState(deviceID, startupState)
		if proxyCfg != nil && proxyCfg.Enabled && strings.TrimSpace(proxyCfg.Addr) != "" {
			probeRes, probeErr := upstreamproxy.ProbeSOCKS5(startCtx, upstreamproxy.ProbeConfig{
				ProxyAddr: proxyCfg.Addr,
				Username:  proxyCfg.Username,
				Password:  proxyCfg.Password,
				Timeout:   5 * time.Second,
			})
			if probeErr != nil {
				if probeRes.UDPAssociationOK() {
					probeSummary := probeRes.FailureSummary()
					startupState.LastReason = "公共 DNS UDP 探测失败，继续验证实际 ePDG/IKE 链路: " + probeSummary
					p.recordVoWiFiStartupState(deviceID, startupState)
					logger.Warn("前置代理公共 DNS UDP 探测失败，继续验证实际 ePDG/IKE 链路",
						"device", deviceID,
						"proxy_addr", proxyCfg.Addr,
						"probe_stage", probeRes.Stage,
						"probe_summary", probeSummary,
						"err", probeErr)
					return nil
				}
				startupState.LastErrorClass = "proxy"
				startupState.LastError = probeErr.Error()
				startupState.LastReason = probeRes.FailureSummary()
				p.recordVoWiFiStartupState(deviceID, startupState)
				return fmt.Errorf("前置代理自检失败: %w", probeErr)
			}
		}
		return nil
	}
}
