package imscore

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/internal/vowifi/imsheaders"
	"github.com/iniwex5/vowifi-go/runtimehost/identity"
)

// New creates an IMS service from the configuration.
func New(cfg *IMSConfig) (*Service, error) {
	if cfg == nil {
		return nil, errors.New("imscore: nil config")
	}
	cfg.syncCompatibilityFields()
	if cfg.IMSNetwork == nil {
		cfg.IMSNetwork = NewSystemIMSNetwork(cfg.LocalIP)
	}
	if cfg.Domain == "" {
		cfg.Domain = "ims.mnc000.mcc000.3gppnetwork.org"
	}
	keepaliveInterval := cfg.KeepaliveInterval
	if keepaliveInterval <= 0 {
		keepaliveInterval = imsKeepaliveInterval
	}
	keepaliveTimeout := cfg.KeepaliveTimeout
	if keepaliveTimeout <= 0 {
		keepaliveTimeout = imsKeepaliveTransactionTimeout
	}
	bus := cfg.EventBus
	if bus == nil {
		bus = newRuntimeEventBus()
	}
	registrarPenalties := cfg.RegistrarPenalties
	if registrarPenalties == nil {
		registrarPenalties = NewRegistrarPenaltyStore()
		cfg.RegistrarPenalties = registrarPenalties
	}
	transport := newSIPTransport()
	registrations := cfg.SubscriptionRegistrations
	if registrations == nil {
		registrations = NewSubscriptionRegistrationStore()
	}
	retries := cfg.SubscriptionRetries
	if retries == nil {
		retries = NewSubscriptionRetryStore()
	}
	s := &Service{
		cfg:                    cfg,
		state:                  regIdle,
		regState:               regIdle,
		dialogRegistry:         newDialogRegistry(),
		bus:                    bus,
		delivery:               cfg.DeliveryStore,
		smsRandom:              cryptorand.Reader,
		stop:                   make(chan struct{}),
		registerErrors:         make(chan error, 1),
		maintenanceWake:        make(chan struct{}, 1),
		downlinkValidationWake: make(chan struct{}, 1),
		protectedConns:         make(map[net.Conn]struct{}),
		transport:              transport,
		registrationRuntime:    registrationRuntime{registrarPenalties: registrarPenalties},
		smsTransactionTimeout:  outboundSMSTransactionTimeout,
		smsReportTimeout:       defaultSMSDeliveryReportTimeout,
		messagingRuntime: messagingRuntime{
			smsPending:     make(map[string]*smsPendingInfo),
			smsPendingNorm: make(map[string]*smsPendingInfo),
			inboundSeen:    make(map[string]time.Time),
			inboundSeenRsp: make(map[string]inboundRequestResponseMemo),
			mtSMSSeen:      make(map[string]time.Time),
			smmaSent:       make(map[string]time.Time),
		},
		fragmentState: fragmentState{
			fragmentCache:          make(map[string][]*smsFragment),
			fragmentRecentExpired:  make(map[string]time.Time),
			fragmentRecentComplete: make(map[string]completedSMSFragmentSession),
		},
		keepaliveInterval:     keepaliveInterval,
		keepaliveTimeout:      keepaliveTimeout,
		keepaliveFailureLimit: imsKeepaliveFailureLimit,
		portSRecoveryJitter:   randomRFC5626RecoveryDelay,

		subscriptionRegistrations: registrations,
		subscriptionRetries:       retries,
	}
	transport.SetFatalHandler(s.handleFatalTransactionError)
	return s, nil
}

// SetOnRegistered wires the registration callback.
func (s *Service) SetOnRegistered(fn func()) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.onRegistered = fn
	s.mu.Unlock()
}

// IsRegistered reports whether the service is registered.
func (s *Service) IsRegistered() bool {
	if s == nil {
		return false
	}
	return s.regStatus.Load() == registrationRegistered
}

// RegState returns the registration state.
func (s *Service) RegState() string {
	if s == nil {
		return regIdle
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.regState
}

// StatusCurrent retains the additive typed status API.
func (s *Service) StatusCurrent() *ServiceStatus {
	status := s.captureStatusSnapshot()
	return &status
}

func (s *Service) captureStatusSnapshot() ServiceStatus {
	if s == nil || s.cfg == nil {
		return ServiceStatus{}
	}
	lastSMSTrace, lastSMSAt, lastSMSErr := s.smsSendStatus()
	inboundStats := s.captureInboundStats()
	diagnostics := inboundStats.diagnostics()
	diagnostics["network"] = s.networkDiagnostics()
	portS := s.capturePortSSession()
	eventBusStatus := s.getIMSEventBus().statusSnapshot()
	s.receiverMu.Lock()
	rxRunning := s.activeReceivers > 0
	s.receiverMu.Unlock()
	s.mu.RLock()
	defer s.mu.RUnlock()
	identities := s.cfg.publicIdentities()
	status := ServiceStatus{
		Enabled: s.cfg.Enabled, DeviceID: s.cfg.DeviceID,
		Registered: s.regState == regRegistered || s.regStatus.Load() == registrationRegistered,
		RegStatus:  registrationStatusText(s.regStatus.Load()),
		Registrar:  s.registrar, RegistrarCandidates: append([]string(nil), s.registrarCandidates...),
		RegistrarIndex: s.registrarIndex, RegistrarSource: s.registrarSource,
		RegistrationGeneration: s.registrationGeneration.Load(),
		RegistrationRegID:      registeredFlowRegIDLocked(s),
		LastSIPCode:            int(s.lastSIPCode.Load()), LastSIPText: s.lastSIPText,
		Domain: s.cfg.Domain, IMPI: s.cfg.IMPI, IMPU: s.cfg.IMPU,
		Transport: s.registrationTransport, SMSReceiverTransport: s.cfg.SMSReceiverTransport(),
		LocalAddr: s.cfg.LocalAddr, LocalPort: s.cfg.LocalPort,
		PortSConnected: portS.connected, PortSGeneration: portS.generation,
		PortSOpenedAt: portS.openedAt, PortSClosedAt: portS.closedAt,
		PortSLastInboundAt: portS.lastInboundAt,
		PortSLastCloseKind: portS.lastCloseKind, PortSLastCloseReason: portS.lastCloseReason,
		PortSPeerResetCount: portS.peerResetCount,
		DeprioritizedPCSCF:  s.registrarPenalties.snapshot(time.Now()),
		IPSecInstalled:      len(s.spiPairs) > 0, RXRunning: rxRunning,
		TCPSignalingRunning:    s.registrationTCP != nil,
		TCPSignalingConnected:  s.registrationTCP != nil,
		EffectiveSecurityMode:  s.effectiveSecurityModeLocked(),
		SecurityFallbackReason: s.securityFallbackReason,
		SecurityFallbackCount:  s.securityFallbackCount.Load(),
		SignalingGeneration:    s.signalingGeneration,
		SignalingReady:         s.signalingReady,
		SignalingFailureReason: s.signalingFailureReason,
		RegFailCount:           int(s.regFailCount.Load()),
		ReRegisterPending:      s.reRegisterPending.Load() || s.notifyReconnectPending.Load(),
		PingFailCount:          int(s.pingFailCount.Load()),
		LastPingAt:             s.lastPingAt, LastPingOK: s.lastPingOK.Load(),
		ServiceRoute: currentServiceRoute(s.regSession),
		Path:         s.path, SecurityVerify: s.securityVerify, AssociatedMSISDN: s.assocMSISDN,
		LastError: s.lastError, LastRegisterTraceID: s.lastRegisterTraceID,
		LastRegisterAttemptAt: s.lastRegisterAttemptAt, LastRegisterOKAt: s.lastRegisterOKAt,
		LastRegisterErr:    s.lastRegisterErr,
		LastSMSSendTraceID: lastSMSTrace, LastSMSSendAt: lastSMSAt,
		LastSMSSendErr: lastSMSErr, FragmentAudit: s.fragmentAuditSnapshot(),
		IMSEventBus: eventBusStatus,
		Diagnostics: diagnostics,
		State:       s.state, RegState: s.regState, IMPUs: identities,
	}
	ready, reason := s.evaluateSignalingReadyLocked(status.Registered)
	status.SignalingReady = status.SignalingReady && ready
	if status.SignalingFailureReason == "" && !ready {
		status.SignalingFailureReason = reason
	}
	return status
}

func currentServiceRoute(session *registerSession) string {
	if session == nil {
		return ""
	}
	return session.serviceRoute
}

func currentPath(session *registerSession, fallback string) string {
	if session != nil {
		if path := strings.TrimSpace(session.path); path != "" {
			return path
		}
	}
	return strings.TrimSpace(fallback)
}

func effectiveIMSRoute(session *registerSession, fallbackPath string) string {
	return imsheaders.EffectiveRoute(currentServiceRoute(session), currentPath(session, fallbackPath))
}

// StatusSnapshotCurrent retains the additive pointer snapshot API.
func (s *Service) StatusSnapshotCurrent() *ServiceStatus {
	return s.StatusCurrent()
}

// DeviceID returns the device ID.
func (s *Service) DeviceID() string {
	if s == nil || s.cfg == nil {
		return ""
	}
	return s.cfg.DeviceID
}

// GetIMSI returns the IMSI.
func (s *Service) GetIMSI() string {
	if s == nil || s.cfg == nil {
		return ""
	}
	return s.cfg.IMSI
}

// GetIMPU returns the IMPU list.
func (s *Service) GetIMPUs() []string {
	if s == nil || s.cfg == nil {
		return nil
	}
	return s.cfg.publicIdentities()
}

// GetIMEI returns the configured mobile equipment identity.
func (s *Service) GetIMEI() string {
	if s == nil || s.cfg == nil {
		return ""
	}
	if strings.TrimSpace(s.cfg.IMEI) == "" {
		return s.cfg.DeviceID
	}
	return s.cfg.IMEI
}

// GetIMSServerAddr returns the IMS server address.
func (s *Service) GetIMSServerAddr() string {
	if s == nil || s.cfg == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if strings.TrimSpace(s.registrar) != "" {
		return s.registrar
	}
	return s.cfg.Registrar
}

// GetLocalIMSAddr returns the local IMS address.
func (s *Service) GetLocalIMSAddr() string {
	if s == nil || s.cfg == nil || s.cfg.LocalIP == nil {
		return ""
	}
	return formatHostPort(s.cfg.LocalIP)
}

// GetLocalPorts returns the local SIP ports.
func (s *Service) GetLocalPortList() []int {
	if s == nil || s.cfg == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ports := []int{s.cfg.LocalPort}
	if s.protectedClientPort > 0 && s.protectedClientPort != s.cfg.LocalPort {
		ports = append(ports, s.protectedClientPort)
	}
	if s.protectedServerPort > 0 && s.protectedServerPort != s.cfg.LocalPort {
		ports = append(ports, s.protectedServerPort)
	}
	return ports
}

// GetRemotePorts returns the remote SIP ports.
func (s *Service) GetRemotePortList() []int {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.regSession != nil && s.regSession.security != nil && s.regSession.security.server != nil {
		return []int{int(s.regSession.security.server.PortC), int(s.regSession.security.server.PortS)}
	}
	if s.registrationRemote != nil {
		return []int{s.registrationRemote.Port}
	}
	return nil
}

// GetPAccessNetworkInfo returns the P-Access-Network-Info header value.
func (s *Service) GetPAccessNetworkInfo() string {
	cfg := s.cfg
	if cfg == nil {
		return ""
	}
	impu := firstNonBlank(cfg.publicIdentities()...)
	if cfg.IMSRegisterTemplate.ID == "" && cfg.IMSRegisterTemplate.FixedPANI == "" &&
		strings.TrimSpace(cfg.PAccessNetworkInfo) == "" {
		seed := stablePANIGenerationSeed([]string{cfg.IMSI, cfg.IMPI, impu, cfg.Domain, cfg.DeviceID})
		return AppendPAccessNetworkCountry(GenerateStablePAccessNetworkInfo(seed), cfg.PAccessNetworkCountry)
	}
	pani := registerPANIForTemplate(cfg.IMSRegisterTemplate, cfg.PAccessNetworkInfo, identity.IMSIdentity{
		ActualSource: identity.IMSIdentitySourceDerived,
		Applied:      true, IMPI: cfg.IMPI, IMPU: impu, Domain: cfg.Domain,
	})
	if strings.TrimSpace(cfg.IMSRegisterTemplate.FixedPANI) != "" ||
		strings.TrimSpace(cfg.PAccessNetworkInfo) != "" {
		return pani
	}
	return AppendPAccessNetworkCountry(pani, cfg.PAccessNetworkCountry)
}

// GetPubGRUU returns the public GRUU.
func (s *Service) GetPubGRUU() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pubGRUU
}

// GetTempGRUU returns the temporary GRUU.
func (s *Service) GetTempGRUU() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tempGRUU
}

// GetRealm returns the digest realm.
func (s *Service) GetRealm() string {
	if s == nil || s.cfg == nil {
		return ""
	}
	if s.cfg.Realm != "" {
		return s.cfg.Realm
	}
	return s.cfg.Domain
}

// GetServiceRoute returns the service route.
func (s *Service) GetServiceRoutes() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if route := effectiveIMSRoute(s.regSession, s.path); route != "" {
		return splitSIPHeaderValues(route)
	}
	return nil
}

// GetSpiPairs returns the IPsec SPI pairs.
func (s *Service) GetSPIPairs() [][2]uint32 {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.spiPairs
}

// GetSecurityVerify returns the Security-Verify header value.
func (s *Service) GetSecurityVerify() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.securityVerify
}

// GetIMSContextSnapshot returns a context snapshot.
func (s *Service) GetIMSContextMap() map[string]interface{} {
	return map[string]interface{}{
		"registered": s.IsRegistered(),
		"reg_state":  s.RegState(),
		"impi":       s.cfg.IMPI,
		"domain":     s.cfg.Domain,
	}
}

// ListenPacket returns a UDP packet connection.
func (s *Service) ListenPacketCurrent(network string, addr *net.UDPAddr) (net.PacketConn, error) {
	if s == nil || s.cfg == nil || s.cfg.IMSNetwork == nil {
		return nil, errors.New("imscore: no network")
	}
	return s.cfg.IMSNetwork.ListenPacket(network, addr)
}

// StopCurrent retains the additive context-free shutdown entrypoint.
func (s *Service) StopCurrent() {
	if s == nil {
		return
	}
	s.setSMSReceiverReady(false)
	if service := s.existingUSSIService(); service != nil {
		service.Stop()
	}
	select {
	case <-s.stop:
	default:
		s.transitionRegStatus(registrationStopping)
		close(s.stop)
	}
	s.stopInboundStatsLogger()
	// The replaced SAs' flows hold readers that networkDone waits for.
	s.retireReplacedSecurity(s.saRetirement.Load(), "stop")
	s.closeProtectedUDP()
	s.cancelPortSReconnectWatch()
	s.clearPendingSMS()
	s.getIMSEventBus().close()
	s.mu.Lock()
	s.cancelReplacementDownlinkWatchLocked()
	registrationIO := s.registrationIO
	registrationTCP := s.registrationTCP
	registrationPreviousTCP := s.registrationPreviousTCP
	registrationPendingTCP := s.registrationPendingTCP
	s.registrationPendingTCP = nil
	securityServerIO := s.securityServerIO
	clientPortReserve := s.clientPortReserve
	s.registrationIO = nil
	s.registrationTCP = nil
	s.registrationPreviousTCP = nil
	s.registrationTCPProtected = false
	s.registrationTransport = ""
	s.registrationRefreshAt = time.Time{}
	s.subscriptionRefreshAt = time.Time{}
	s.subscriptionClosed = true
	s.subscriptionDialog = registrationSubscriptionDialog{}
	s.mwiSubscriptionRefreshAt = time.Time{}
	s.mwiSubscriptionClosed = true
	s.mwiSubscriptionDialog = registrationSubscriptionDialog{}
	s.signalingReady = false
	s.securityServerIO = nil
	s.clientPortReserve = nil
	s.mu.Unlock()
	if s.transport != nil {
		_ = s.transport.Close()
	}
	if registrationIO != nil {
		_ = registrationIO.Close()
	}
	if registrationTCP != nil {
		_ = registrationTCP.Close()
	}
	if registrationPreviousTCP != nil && registrationPreviousTCP != registrationTCP {
		_ = registrationPreviousTCP.Close()
	}
	if registrationPendingTCP != nil {
		_ = registrationPendingTCP.Close()
	}
	if securityServerIO != nil {
		_ = securityServerIO.Close()
	}
	if clientPortReserve != nil {
		_ = clientPortReserve.Close()
	}
	s.protectedConnMu.Lock()
	for conn := range s.protectedConns {
		_ = conn.Close()
	}
	s.protectedConnMu.Unlock()
	s.networkDone.Wait()
	if dialogs := s.dialogs(); dialogs != nil {
		dialogs.closeAll()
	}
	s.clearServerTransactions()
	if closer, ok := s.cfg.IMSNetwork.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
	s.mu.Lock()
	s.spiPairs = nil
	s.securityVerify = ""
	s.pubGRUU = ""
	s.tempGRUU = ""
	s.regState = regUnregister
	s.mu.Unlock()
	s.transitionRegStatus(registrationStopped)
	s.notifySMSReadiness()
}

// RegistrationErrors reports background refresh failures.
func (s *Service) RegistrationErrors() <-chan error {
	if s == nil {
		return nil
	}
	return s.registerErrors
}

// TriggerRegisterImmediate performs an immediate re-registration and exposes
// the real result to its caller.
func (s *Service) TriggerRegisterImmediateCurrent() error {
	if s == nil {
		return errors.New("imscore: nil service")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.Register(ctx)
}
