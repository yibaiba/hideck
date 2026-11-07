package runtimehost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/engine/ipsec"
	"github.com/iniwex5/vowifi-go/engine/swu"
	"github.com/iniwex5/vowifi-go/internal/vowifi/common"
	"github.com/iniwex5/vowifi-go/internal/vowifi/epdg"
	"github.com/iniwex5/vowifi-go/internal/vowifi/imscore"
	"github.com/iniwex5/vowifi-go/internal/vowifi/logging"
	"github.com/iniwex5/vowifi-go/internal/vowifi/netstack"
	"github.com/iniwex5/vowifi-go/internal/vowifi/profile"
	"github.com/iniwex5/vowifi-go/internal/vowifi/runtimecore"
	"github.com/iniwex5/vowifi-go/runtimehost/eventhost"
	"github.com/iniwex5/vowifi-go/runtimehost/identity"
	"github.com/iniwex5/vowifi-go/runtimehost/messaging"
	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
)

const (
	imsRegistrationTimeout   = 30 * time.Second
	epdgEstablishmentTimeout = 45 * time.Second
	defaultIMSSIPPort        = 5060
)

// StartMode preserves the additive name without changing the recovered string field.
type StartMode = string

const (
	// StartModeMain runs the full IMS host.
	StartModeMain = "main"
	// StartModeReader runs a SIM-reader-only host.
	StartModeReader = "reader"
)

// StartRequest configures a runtime host start.
type StartRequest struct {
	Mode          string
	DeviceID      string
	TraceID       string
	Profile       identity.Profile
	Prepared      *identity.PreparedSession
	IMSIdentity   identity.IMSIdentityResult
	NetworkMode   string
	VoiceGateway  *voicehost.Gateway
	SIM           SIMAdapter
	Access        identity.AccessAdapter
	Dataplane     DataplanePolicy
	Proxy         *ProxyConfig
	DNSServer     string
	DeliveryStore messaging.DeliveryStore
	Dispatch      eventhost.Dispatcher
	BeforeStart   func(context.Context, SessionConfig) error
	ShouldRun     func() bool
	runner        func(context.Context, runtimecore.RuntimeStartRequest) (StartResult, error)
	Observer      Observer

	TunnelFactory TunnelFactory
	IMSFactory    IMSFactory
}

// ModemAccessAdapter preserves the current name for the identity access ABI.
type ModemAccessAdapter = identity.AccessAdapter

// NewModemAccessAdapter adapts a Modem to the ModemAccessAdapter surface.
func NewModemAccessAdapter(m Modem) ModemAccessAdapter {
	if m == nil {
		return nil
	}
	return modemAccessAdapter{modem: m}
}

// Start launches a runtime host for the given request and returns the Instance.
func Start(ctx context.Context, req StartRequest) (*Instance, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateCommonStartRequest(req); err != nil {
		return nil, err
	}
	if req.TunnelFactory != nil || req.IMSFactory != nil {
		return startCurrent(ctx, req)
	}
	waitForReady, delay := req.startPolicy()
	coreRequest := req.coreRequest()
	launch := runtimeLaunchOptions{runner: req.runner, delay: delay, observer: req.Observer}
	if !waitForReady {
		instance, err := startInstanceAsync(ctx, coreRequest, launch)
		applyRequestMetadata(instance, req)
		return instance, err
	}
	instance, err := startInstance(ctx, coreRequest, launch)
	applyRequestMetadata(instance, req)
	return instance, err
}

func (req StartRequest) startPolicy() (bool, reconnectDelay) {
	if req.Mode == StartModeReader {
		return false, defaultReaderReconnectDelay
	}
	return true, defaultMainReconnectDelay
}

func startCurrent(ctx context.Context, req StartRequest) (*Instance, error) {
	if req.ShouldRun != nil && !req.ShouldRun() {
		return nil, errors.New("runtimehost: start cancelled by ShouldRun")
	}
	if err := validateCurrentStartRequest(req); err != nil {
		return nil, err
	}
	if req.BeforeStart != nil {
		if err := req.BeforeStart(ctx, currentSessionConfig(ctx, req)); err != nil {
			return nil, err
		}
	}

	inst := &Instance{}
	inst.setState(initialState(req))
	inst.AddObserver(req.Observer)
	runCtx, cancel := context.WithCancel(ctx)
	tunnel, err := newTunnel(req, inst)
	if err != nil {
		cancel()
		inst.setStartFailure(err)
		return inst, err
	}
	inst.attachTunnel(tunnel, cancel)
	if err := tunnel.Connect(runCtx); err != nil {
		startErr := fmt.Errorf("runtimehost: SWu tunnel establishment failed: %w", err)
		_ = inst.Stop(context.Background())
		inst.setStartFailure(startErr)
		return inst, startErr
	}
	inst.markTunnelReadyForIMS()
	ims, err := newIMS(req, tunnel)
	if err != nil {
		return failIMSStart(inst, err)
	}
	inst.setService(lifecycleServiceAdapter{lifecycle: ims})
	wireSMSReadiness(inst, ims)
	registrationCtx, registrationCancel := context.WithTimeout(runCtx, imsRegistrationTimeout)
	err = ims.Register(registrationCtx)
	registrationCancel()
	if err != nil {
		return failIMSStart(inst, fmt.Errorf("runtimehost: IMS registration failed: %w", err))
	}
	inst.markIMSRegistered()
	syncSMSReadiness(inst, ims)
	if err := attachVoiceAgent(req, inst, ims); err != nil {
		return failIMSStart(inst, fmt.Errorf("runtimehost: attach voice agent: %w", err))
	}
	go monitorTunnelFailure(runCtx, inst, tunnel)
	go monitorRegistrationFailures(runCtx, inst, ims)
	go stopRuntimeOnContext(runCtx, inst)
	return inst, nil
}

func applyRequestMetadata(instance *Instance, request StartRequest) {
	if instance == nil {
		return
	}
	instance.mu.Lock()
	instance.state.NetworkMode = request.NetworkMode
	instance.state.EPDGAddress = epdgOf(request)
	if instance.state.DataplaneMode == "" {
		instance.state.DataplaneMode = request.Dataplane.Mode
	}
	instance.mu.Unlock()
}

func monitorTunnelFailure(ctx context.Context, inst *Instance, tunnel Tunnel) {
	waitErr := tunnel.WaitDoneContext(ctx)
	if ctx.Err() != nil {
		return
	}
	failureErr := waitErr
	if source, ok := tunnel.(tunnelFailureSource); ok {
		if terminalErr := source.TerminalError(); terminalErr != nil {
			failureErr = terminalErr
		}
	}
	if failureErr == nil {
		failureErr = errors.New("SWu tunnel stopped unexpectedly")
	}
	wrapped := fmt.Errorf("runtimehost: SWu tunnel control failed: %w", failureErr)
	if errors.Is(failureErr, swu.ErrFreshRuntimeRequired) {
		logger.Info("SWu IKE 重鉴权请求新运行时", "device", inst.State().DeviceID, "err", wrapped)
		inst.setTunnelReauthenticationRequired(wrapped)
		return
	}
	logger.Error("SWu tunnel control failed", "device", inst.State().DeviceID, "err", wrapped)
	inst.setTunnelControlFailure(wrapped)
}

func wireSMSReadiness(inst *Instance, ims IMSLifecycle) {
	source, ok := ims.(smsReadinessSource)
	if !ok {
		return
	}
	source.SetOnSMSReadinessChanged(func(readiness SMSReadiness) {
		inst.updateSMSReadiness(readiness)
	})
}

func syncSMSReadiness(inst *Instance, ims IMSLifecycle) {
	if source, ok := ims.(smsReadinessSource); ok {
		inst.updateSMSReadiness(source.SMSReadiness())
	}
}

func monitorRegistrationFailures(ctx context.Context, inst *Instance, ims IMSLifecycle) {
	source, ok := ims.(registrationFailureSource)
	if !ok || source.RegistrationErrors() == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case err, ok := <-source.RegistrationErrors():
			if !ok {
				return
			}
			if err != nil {
				logger.Error("IMS registration refresh failed", "device", inst.State().DeviceID, "err", err)
				inst.setIMSRefreshFailure(err)
			}
		}
	}
}

func failIMSStart(inst *Instance, err error) (*Instance, error) {
	_ = inst.Stop(context.Background())
	inst.setIMSFailure(err)
	return inst, err
}

func validateCommonStartRequest(req StartRequest) error {
	if strings.TrimSpace(req.DeviceID) == "" {
		return errors.New("runtimehost: device_id is empty")
	}
	if req.SIM == nil || req.SIM.runtimeSIMAdapter() == nil {
		return errors.New("runtimehost: SIM AKA provider is required")
	}
	mode := strings.TrimSpace(req.Dataplane.Mode)
	if mode != "" && mode != swu.DataplaneModeUserspace {
		return fmt.Errorf("runtimehost: unsupported dataplane mode %q", mode)
	}
	if req.Proxy != nil && req.Proxy.Enabled && strings.TrimSpace(req.Proxy.Addr) == "" {
		return errors.New("runtimehost: enabled SOCKS5 proxy has no address")
	}
	return nil
}

func validateCurrentStartRequest(req StartRequest) error {
	if req.Prepared == nil {
		return errors.New("runtimehost: prepared session is required")
	}
	if strings.TrimSpace(req.Prepared.EPDGAddr) == "" {
		return errors.New("runtimehost: prepared session has no ePDG address")
	}
	provider := req.SIM.runtimeSIMAdapter().EPDGSIMProvider(runtimeAuthPlan(req.Prepared))
	if provider == nil {
		return errors.New("runtimehost: SIM AKA provider is required")
	}
	return nil
}

func currentSessionConfig(ctx context.Context, req StartRequest) SessionConfig {
	return SessionConfig{
		Ctx: ctx, DeviceID: req.DeviceID, TraceID: req.TraceID, Prepared: *req.Prepared,
		DataplaneMode: req.Dataplane.Mode, TUNName: req.Dataplane.TUNName,
		Proxy: req.Proxy, DNSServer: req.DNSServer,
	}
}

func initialState(req StartRequest) State {
	return State{
		Phase: "starting", SessionState: "starting", LastEvent: "starting",
		DeviceID: req.DeviceID, EPDGAddress: epdgOf(req), NetworkMode: req.NetworkMode,
		DataplaneMode: req.Dataplane.Mode, SIMReady: true, AccessReady: req.Access != nil,
	}
}

// NewDefaultTunnel creates the default SWu tunnel adapter, allowing external
// callers to wrap or pre-configure the swu.Config before using the standard
// adapter. This is used by cellular mode to inject a TransportFactory that
// binds the tunnel socket to a specific network interface.
func NewDefaultTunnel(deviceID string, cfg *swu.Config) (Tunnel, error) {
	return newSWUTunnelAdapter(swuTunnelAdapterConfig{
		Manager:              epdg.New(),
		DeviceID:             deviceID,
		SessionConfig:        cfg,
		EstablishmentTimeout: epdgEstablishmentTimeout,
	}), nil
}

func newTunnel(req StartRequest, inst *Instance) (Tunnel, error) {
	prepared := preparedForRuntimeCore(req.Prepared)
	provider := req.SIM.runtimeSIMAdapter().EPDGSIMProvider(runtimeAuthPlan(req.Prepared))
	cfg, err := runtimecore.BuildCompatibilitySWUConfig(prepared, provider)
	if err != nil {
		return nil, err
	}
	cfg.DeviceID = req.DeviceID
	cfg.OnStateChange = inst.updateTunnelState
	if req.Proxy != nil && req.Proxy.Enabled {
		cfg.ProxyAddr = strings.TrimSpace(req.Proxy.Addr)
		cfg.Proxy = &ipsec.Socks5Config{Username: req.Proxy.Username, Password: req.Proxy.Password}
	}
	factory := req.TunnelFactory
	if factory == nil {
		factory = func(cfg *swu.Config) (Tunnel, error) {
			return newSWUTunnelAdapter(swuTunnelAdapterConfig{
				Manager: epdg.New(), DeviceID: req.DeviceID, SessionConfig: cfg,
				EstablishmentTimeout: epdgEstablishmentTimeout,
			}), nil
		}
	}
	tunnel, err := factory(cfg)
	if err != nil {
		return nil, fmt.Errorf("runtimehost: create SWu tunnel: %w", err)
	}
	if tunnel == nil {
		return nil, errors.New("runtimehost: create SWu tunnel: nil session")
	}
	return tunnel, nil
}

func preparedForRuntimeCore(prepared *identity.PreparedSession) *runtimecore.PreparedSessionStart {
	authPlan := runtimeAuthPlan(prepared)
	return &runtimecore.PreparedSessionStart{
		Profile: profile.Profile{
			ICCID: prepared.Profile.ICCID, IMSI: prepared.Profile.IMSI, MCC: prepared.Profile.MCC, MNC: prepared.Profile.MNC,
			IMEI: prepared.Profile.IMEI, UserAgent: prepared.Profile.UserAgent,
			SMSC: prepared.Profile.SMSC, IMSDomain: prepared.Profile.IMSDomain,
		},
		IMSIdentity: profile.IMSIdentity{
			IMPI: prepared.IMSIdentity.IMPI, IMPU: []string{prepared.IMSIdentity.IMPU},
			Domain: prepared.IMSIdentity.Domain,
		},
		IMSIdentityResult: profile.IMSIdentityResult{
			RequestedSource:  string(prepared.IMSIdentity.RequestedSource),
			ActualSource:     string(prepared.IMSIdentity.ActualSource),
			AKAAppPreference: string(prepared.IMSIdentity.AKAAppPreference),
			Applied:          prepared.IMSIdentity.Applied,
			IMPI:             prepared.IMSIdentity.IMPI,
			IMPU:             prepared.IMSIdentity.IMPU,
			Domain:           prepared.IMSIdentity.Domain,
		},
		AuthPlan: authPlan,
		EPDGAddr: prepared.EPDGAddr, EPDGSource: prepared.EPDGSource,
		APN:                imsAPNFromDomain(prepared.IMSIdentity.Domain),
		Carrier:            prepared.ResolvedCarrierConfig(),
		IdentityIMEISource: prepared.IdentityIMEISource,
	}
}

func runtimeAuthPlan(prepared *identity.PreparedSession) profile.AuthPlan {
	if prepared == nil {
		return profile.AuthPlan{}
	}
	if prepared.AuthPlan != (identity.AuthPlan{}) {
		return authPlanToInternal(prepared.AuthPlan).Normalize()
	}
	return authPlanForPreference(prepared.IMSIdentity.AKAAppPreference)
}

func authPlanForPreference(preference string) profile.AuthPlan {
	normalized := profile.NormalizeAKAApp(preference)
	plan := profile.NewAuthPlan(normalized, normalized)
	plan.AKAApp = normalized
	return plan
}

func imsAPNFromDomain(domain string) string {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return ""
	}
	apn, _, _ := strings.Cut(domain, ".")
	return strings.TrimSpace(apn)
}

func stopRuntimeOnContext(ctx context.Context, inst *Instance) {
	<-ctx.Done()
	_ = inst.Stop(context.Background())
}

func newIMS(req StartRequest, tunnel Tunnel) (IMSLifecycle, error) {
	if req.IMSFactory != nil {
		ims, err := req.IMSFactory(req, tunnel)
		if err != nil {
			return nil, fmt.Errorf("runtimehost: create IMS service: %w", err)
		}
		if ims == nil {
			return nil, errors.New("runtimehost: create IMS service: nil lifecycle")
		}
		return ims, nil
	}
	svc, err := imscoreFromPrepared(req, tunnel)
	if err != nil {
		return nil, err
	}
	return &imscoreLifecycleAdapter{svc: svc}, nil
}

// imscoreFromPrepared builds an imscore.Service from the prepared session.
func imscoreFromPrepared(req StartRequest, tunnel Tunnel) (*imscore.Service, error) {
	ident := req.Prepared.IMSIdentity
	carrierConfig := req.Prepared.ResolvedCarrierConfig()
	domain := firstNonEmptyString(
		ident.Domain, carrierConfig.IMSDomain, req.Prepared.Profile.IMSDomain,
	)
	impi := strings.TrimSpace(ident.IMPI)
	if impi == "" && req.Prepared.Profile.IMSI != "" && domain != "" {
		impi = strings.TrimSpace(req.Prepared.Profile.IMSI) + "@" + domain
	}
	if domain == "" || impi == "" {
		return nil, errors.New("runtimehost: prepared IMS identity is incomplete")
	}
	impu := strings.TrimSpace(ident.IMPU)
	if impu == "" {
		impu = "sip:" + impi
	}
	inner := tunnel.InnerNetwork()
	innerIP, prefixLen := preferredInnerAddress(inner)
	if innerIP == nil || tunnel.InnerPacketIO() == nil {
		return nil, errors.New("runtimehost: SWu tunnel has no usable inner packet network")
	}
	logging.Info("IMS inner address selected",
		"device", req.DeviceID, "ip", innerIP.String(), "prefix", prefixLen,
		"ipv4", inner.IPv4.To4() != nil,
		"ipv6", inner.IPv6 != nil && inner.IPv6.To4() == nil,
		"ipv6_skip", inner.IPv6IMSSkipReason())
	dns := common.ToStrings(inner.DNS)
	imsNetwork, err := netstack.NewTunnelNetwork(innerIP, prefixLen, dns, tunnel.InnerPacketIO())
	if err != nil {
		return nil, fmt.Errorf("runtimehost: create IMS tunnel network: %w", err)
	}
	registrar := ""
	if pcscf := preferredPCSCF(inner.PCSCF, innerIP); pcscf != nil {
		registrar = net.JoinHostPort(pcscf.String(), fmt.Sprint(defaultIMSSIPPort))
	}
	registerTemplate, userAgent, err := imsRegisterConfigForPrepared(req.Prepared)
	if err != nil {
		_ = imsNetwork.Close()
		return nil, err
	}
	cfg := &imscore.IMSConfig{
		DeviceID: req.DeviceID,
		ICCID:    strings.TrimSpace(req.Prepared.Profile.ICCID),
		IMEI: firstNonEmptyString(
			req.Prepared.Profile.IMEI,
			imscore.GenerateRandomIMEIForModel(defaultIMSDeviceModel),
		),
		IMSI:        firstNonEmptyString(req.Prepared.Profile.IMSI, imsiOf(impi)),
		IMPI:        impi,
		IMPU:        impu,
		Domain:      domain,
		SMSC:        strings.TrimSpace(req.Prepared.Profile.SMSC),
		Realm:       domain,
		EPDGAddr:    req.Prepared.EPDGAddr,
		LocalIP:     innerIP,
		Registrar:   registrar,
		Transport:   registerTemplate.Transport,
		Expires:     registerTemplate.Expires,
		TraceID:     req.TraceID,
		AKAProvider: req.SIM.runtimeSIMAdapter().EPDGSIMProvider(runtimeAuthPlan(req.Prepared)),
		IMSNetwork:  imsNetwork,
		UserAgent:   userAgent,
		CellularNetworkInfo: imscore.GenerateDefaultCellularNetworkInfo(
			req.Prepared.Profile.MCC, req.Prepared.Profile.MNC,
		),
		PAccessNetworkCountry: imscore.CountryISO2FromMCC(req.Prepared.Profile.MCC),
		RegisterTemplate:      registerTemplate,
		OnLocalAddressChange: func(oldIP, newIP net.IP) error {
			return tunnel.UpdateAddresses(oldIP, newIP)
		},
	}
	cfg.SetEnableIPSec3GPP(true)
	eventBus := imscore.NewEventBus()
	cfg.EventBus = eventBus
	if req.Dispatch != nil {
		eventBus.Subscribe(&imsEventBridge{dispatcher: req.Dispatch})
	}
	applyDeliveryStores(cfg, req.DeliveryStore)
	svc, err := imscore.New(cfg)
	if err != nil {
		_ = imsNetwork.Close()
		return nil, err
	}
	return svc, nil
}

// applyDeliveryStores wires the host store into a custom-tunnel IMS service the
// same way the runtimecore path does.
func applyDeliveryStores(cfg *imscore.IMSConfig, store messaging.DeliveryStore) {
	if store == nil {
		return
	}
	cfg.DeliveryStore = newDeliveryStoreAdapter(store)
	cfg.SubscriptionRegistrations = imscore.NewPersistentSubscriptionRegistrationStore(
		runtimeCoreSubscriptionRejectionStore(store),
	)
	cfg.MWIPolicyStore = runtimeCoreMWIPolicyStore(store)
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func preferredPCSCF(servers []net.IP, innerIP net.IP) net.IP {
	wantIPv4 := innerIP.To4() != nil
	for _, server := range servers {
		if (server.To4() != nil) == wantIPv4 {
			return server
		}
	}
	return nil
}

func preferredInnerAddress(inner swu.InnerNetworkConfig) (net.IP, int) {
	return inner.PreferredIMSAddress()
}

// imsiOf extracts the IMSI from an IMPI (the part before '@').
func imsiOf(impi string) string {
	if i := strings.IndexByte(impi, '@'); i > 0 {
		return impi[:i]
	}
	return impi
}

// epdgOf returns the ePDG address from the prepared session.
func epdgOf(req StartRequest) string {
	if req.Prepared != nil {
		return req.Prepared.EPDGAddr
	}
	return ""
}

// WithTraceID returns a context carrying the given trace id.
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return common.WithTraceID(ctx, traceID)
}
