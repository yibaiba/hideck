package runtimehost

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	enginesim "github.com/iniwex5/vowifi-go/engine/sim"
	"github.com/iniwex5/vowifi-go/engine/swu"
	"github.com/iniwex5/vowifi-go/internal/vowifi/access"
	"github.com/iniwex5/vowifi-go/internal/vowifi/runtimecore"
	"github.com/iniwex5/vowifi-go/runtimehost/identity"
)

// stubAccess is a minimal identity.Access for tests.
type stubAccess struct{}

func (stubAccess) Capabilities() identity.Capabilities { return identity.Capabilities{HasISIM: true} }
func (stubAccess) IMSIdentityProvider() identity.IMSIdentityProvider {
	return stubProvider{}
}

type stubProvider struct{}

func (stubProvider) GetISIMIdentity() (identity.Identity, error) {
	return identity.Identity{
		IMSI:   "310260123456789",
		IMPI:   "310260123456789@ims.mnc026.mcc310.3gppnetwork.org",
		IMPU:   []string{"sip:310260123456789@ims.mnc026.mcc310.3gppnetwork.org"},
		Domain: "ims.mnc026.mcc310.3gppnetwork.org",
	}, nil
}

type startAKAProvider struct{}

func (startAKAProvider) CalculateAKA(_, _ []byte) (enginesim.AKAResult, error) {
	return enginesim.AKAResult{RES: []byte{1}, CK: make([]byte, 16), IK: make([]byte, 16)}, nil
}

type startSIMAdapter struct{}

func (startSIMAdapter) runtimeSIMAdapter() access.SIMAdapter {
	return runtimecore.NewReaderSIMAdapter(startAKAProvider{})
}

type lifecycleTunnel struct {
	mu            sync.Mutex
	state         string
	connectErr    error
	done          chan struct{}
	connectCalled chan struct{}
	shutdownOnce  sync.Once
	onStateChange func(string)
	packetIO      *lifecyclePacketIO
	updateErr     error
	terminalErr   error
	oldIP         net.IP
	newIP         net.IP
	onShutdown    func()
}

type lifecycleIMS struct {
	CurrentService
	registerErr error
	deviceID    string
	registered  bool
	stopped     bool
	started     chan struct{}
	release     chan struct{}
	refreshErrs chan error
	sms         SMSReadiness
	smsObserver func(SMSReadiness)
	onStop      func()
}

func (s *lifecycleIMS) Register(ctx context.Context) error {
	if s.started != nil {
		close(s.started)
	}
	if s.release != nil {
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if s.registerErr != nil {
		return s.registerErr
	}
	s.registered = true
	return nil
}

func (s *lifecycleIMS) Stop() {
	s.stopped = true
	if s.onStop != nil {
		s.onStop()
	}
}

func (s *lifecycleIMS) Status() Status {
	return Status{State: State{DeviceID: s.deviceID, IMSReady: s.registered}}
}

func (s *lifecycleIMS) StatusSnapshot() Status { return s.Status() }

func (s *lifecycleIMS) RegistrationErrors() <-chan error { return s.refreshErrs }

func (s *lifecycleIMS) SMSReadiness() SMSReadiness { return s.sms }

func (s *lifecycleIMS) SetOnSMSReadinessChanged(fn func(SMSReadiness)) {
	s.smsObserver = fn
	if fn != nil {
		fn(s.sms)
	}
}

func (s *lifecycleIMS) setSMSReadiness(readiness SMSReadiness) {
	s.sms = readiness
	if s.smsObserver != nil {
		s.smsObserver(readiness)
	}
}

type lifecyclePacketIO struct{}

func (*lifecyclePacketIO) ReadPacketContext(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*lifecyclePacketIO) WritePacketContext(context.Context, []byte) error { return nil }

func newLifecycleTunnel(connectErr error) *lifecycleTunnel {
	return &lifecycleTunnel{
		state: "idle", connectErr: connectErr, done: make(chan struct{}), connectCalled: make(chan struct{}),
		packetIO: &lifecyclePacketIO{},
	}
}

func (t *lifecycleTunnel) Connect(context.Context) error {
	t.setState("connecting")
	close(t.connectCalled)
	if t.connectErr != nil {
		t.setState("error")
		return t.connectErr
	}
	t.setState("established")
	return nil
}

func (t *lifecycleTunnel) Shutdown() {
	t.shutdownOnce.Do(func() {
		if t.onShutdown != nil {
			t.onShutdown()
		}
		t.setState("shutdown")
		close(t.done)
	})
}

func (t *lifecycleTunnel) State() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

func (t *lifecycleTunnel) WaitDoneContext(ctx context.Context) error {
	select {
	case <-t.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *lifecycleTunnel) TerminalError() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.terminalErr
}

func (t *lifecycleTunnel) fail(err error) {
	t.mu.Lock()
	t.terminalErr = err
	t.mu.Unlock()
	t.setState("error")
	t.shutdownOnce.Do(func() { close(t.done) })
}

func (t *lifecycleTunnel) InnerNetwork() swu.InnerNetworkConfig {
	return swu.InnerNetworkConfig{
		IPv4: net.IPv4(10, 0, 0, 2), PrefixLen: 32, DNS: []net.IP{net.IPv4(10, 0, 0, 1)},
	}
}

func (t *lifecycleTunnel) InnerPacketIO() swu.InnerPacketIO { return t.packetIO }

func (t *lifecycleTunnel) UpdateAddresses(oldIP, newIP net.IP) error {
	t.oldIP = append(net.IP(nil), oldIP...)
	t.newIP = append(net.IP(nil), newIP...)
	return t.updateErr
}

func (t *lifecycleTunnel) setState(state string) {
	t.mu.Lock()
	t.state = state
	t.mu.Unlock()
	if t.onStateChange != nil {
		t.onStateChange(state)
	}
}

func runtimeTestRequest(prepared *identity.PreparedSession, tunnel *lifecycleTunnel) StartRequest {
	return StartRequest{
		Mode: StartModeMain, DeviceID: "wwan0", TraceID: "trace-1", Prepared: prepared,
		SIM: startSIMAdapter{}, Dataplane: DataplanePolicy{Mode: swu.DataplaneModeUserspace},
		TunnelFactory: func(cfg *swu.Config) (Tunnel, error) {
			tunnel.onStateChange = cfg.OnStateChange
			return tunnel, nil
		},
		IMSFactory: func(req StartRequest, _ Tunnel) (IMSLifecycle, error) {
			return &lifecycleIMS{deviceID: req.DeviceID, sms: SMSReadiness{
				Registered: true, ProfileReady: true, TransportReady: true, ReceiverReady: true, SMSCPresent: true,
				Ready: true, Reason: "IMS SMS receiver ready",
			}}, nil
		},
	}
}

func TestStartSMSReadyTracksReportedPrerequisites(t *testing.T) {
	prepared := &identity.PreparedSession{
		Profile:     identity.Profile{IMSI: "310260123456789", SMSC: "+123"},
		IMSIdentity: identity.IMSIdentity{IMPI: "310260123456789@ims.example", IMPU: "sip:310260123456789@ims.example", Domain: "ims.example"},
		EPDGAddr:    "epdg.example.com",
	}
	ims := &lifecycleIMS{deviceID: "dev-1", sms: SMSReadiness{
		Registered: true, SMSCPresent: true, Reason: "IMS SMS receiver is not ready",
	}}
	req := runtimeTestRequest(prepared, newLifecycleTunnel(nil))
	req.IMSFactory = func(StartRequest, Tunnel) (IMSLifecycle, error) { return ims, nil }
	inst, err := Start(context.Background(), req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if state := inst.State(); state.SMSReady || state.SMSReadyReason != "IMS SMS receiver is not ready" {
		t.Fatalf("initial SMS state = %+v", state)
	}
	ims.setSMSReadiness(SMSReadiness{
		Registered: true, ProfileReady: true, TransportReady: true, ReceiverReady: true, SMSCPresent: true,
		Ready: true, Reason: "IMS SMS receiver ready",
	})
	if state := inst.State(); !state.SMSReady || state.SMSReadyReason != "IMS SMS receiver ready" {
		t.Fatalf("updated SMS state = %+v", state)
	}
	ims.setSMSReadiness(SMSReadiness{
		Registered: true, ProfileReady: true, TransportReady: true, SMSCPresent: true,
		Reason: "IMS SMS receiver is not ready",
	})
	if state := inst.State(); state.SMSReady || state.Phase != "ims_ready" || state.LastEvent != "sms_unavailable" {
		t.Fatalf("unavailable SMS state = %+v", state)
	}
}

func TestPreferredInnerAddressUsesIPv6WhenUsable(t *testing.T) {
	inner := swu.InnerNetworkConfig{
		IPv4: net.ParseIP("192.0.2.8"), PrefixLen: 32,
		IPv6: net.ParseIP("2001:db8::8"), IPv6PrefixLen: 64,
		PCSCF: []net.IP{net.ParseIP("192.0.2.20"), net.ParseIP("2001:db8::20")},
	}
	address, prefixLen := preferredInnerAddress(inner)
	if !address.Equal(inner.IPv6) || prefixLen != 64 {
		t.Fatalf("preferred inner address = %s/%d", address, prefixLen)
	}
	if reason := inner.IPv6IMSSkipReason(); reason != "" {
		t.Fatalf("ipv6 skip = %q", reason)
	}
}

func TestPreferredInnerAddressSkipsIPv6WithoutPCSCF(t *testing.T) {
	inner := swu.InnerNetworkConfig{
		IPv4: net.ParseIP("192.0.2.8"), PrefixLen: 32,
		IPv6: net.ParseIP("2001:db8::8"), IPv6PrefixLen: 64,
		PCSCF: []net.IP{net.ParseIP("192.0.2.20")},
	}
	address, prefixLen := preferredInnerAddress(inner)
	if !address.Equal(inner.IPv4) || prefixLen != 32 {
		t.Fatalf("preferred inner address = %s/%d", address, prefixLen)
	}
	if reason := inner.IPv6IMSSkipReason(); reason != "no_ipv6_pcscf" {
		t.Fatalf("ipv6 skip = %q", reason)
	}
}

func TestPreferredInnerAddressUsesIPv6WhenPCSCFIsIPv6Only(t *testing.T) {
	inner := swu.InnerNetworkConfig{
		IPv4: net.ParseIP("192.0.2.8"), PrefixLen: 32,
		IPv6: net.ParseIP("2001:db8::8"), IPv6PrefixLen: 64,
		PCSCF: []net.IP{net.ParseIP("2001:db8::20")},
	}
	address, prefixLen := preferredInnerAddress(inner)
	if !address.Equal(inner.IPv6) || prefixLen != 64 {
		t.Fatalf("preferred inner address = %s/%d", address, prefixLen)
	}
}

func TestPreferredInnerAddressFallsBackToIPv4(t *testing.T) {
	inner := swu.InnerNetworkConfig{IPv4: net.ParseIP("192.0.2.8"), PrefixLen: 32}
	address, prefixLen := preferredInnerAddress(inner)
	if !address.Equal(inner.IPv4) || prefixLen != 32 {
		t.Fatalf("preferred inner address = %s/%d", address, prefixLen)
	}
}

func TestPreferredPCSCFMatchesTunnelFamily(t *testing.T) {
	servers := []net.IP{net.ParseIP("192.0.2.20"), net.ParseIP("2001:db8::20")}
	got := preferredPCSCF(servers, net.ParseIP("2001:db8::8"))
	if !got.Equal(net.ParseIP("2001:db8::20")) {
		t.Fatalf("preferred P-CSCF = %s", got)
	}
}

func TestStart(t *testing.T) {
	prepared, err := identity.PrepareStart(identity.PrepareStartInput{
		DeviceID: "wwan0",
		Profile:  identity.Profile{IMSI: "310260123456789", MCC: "310", MNC: "26"},
		Access:   stubAccess{},
	})
	if err != nil {
		t.Fatalf("PrepareStart: %v", err)
	}
	var beforeCalled bool
	tunnel := newLifecycleTunnel(nil)
	req := runtimeTestRequest(&prepared, tunnel)
	req.Profile = prepared.Profile
	req.BeforeStart = func(_ context.Context, cfg SessionConfig) error {
		beforeCalled = cfg.DataplaneMode == swu.DataplaneModeUserspace
		return nil
	}
	req.ShouldRun = func() bool { return true }
	inst, err := Start(context.Background(), req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !beforeCalled {
		t.Error("BeforeStart not called")
	}
	if inst.State().SessionState != "established" {
		t.Errorf("state = %+v", inst.State())
	}
	if inst.State().EPDGAddress == "" {
		t.Error("EPDG address not set from prepared session")
	}
	select {
	case <-tunnel.connectCalled:
	default:
		t.Error("SWu Connect was not called")
	}
	if !inst.State().TunnelReady || !inst.State().DataPlaneUp {
		t.Errorf("tunnel state = %+v", inst.State())
	}
	if err := inst.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestStartShouldRunFalse(t *testing.T) {
	if _, err := Start(context.Background(), StartRequest{
		ShouldRun: func() bool { return false },
	}); err == nil {
		t.Error("Start with ShouldRun=false should error")
	}
}

func TestStartDefaultRoutesThroughRuntimeCorePreparation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := Start(ctx, StartRequest{
		DeviceID: "dev-1",
		Prepared: &identity.PreparedSession{
			EPDGAddr: "epdg.example.com",
		},
		SIM:       startSIMAdapter{},
		Dataplane: DataplanePolicy{Mode: swu.DataplaneModeUserspace},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start error = %v, want bounded reconnect cancellation", err)
	}
}

func TestNewModemAccessAdapter(t *testing.T) {
	if NewModemAccessAdapter(nil) != nil {
		t.Error("nil modem should produce nil adapter")
	}
}

type readerCapabilitiesModem struct{}

func (readerCapabilitiesModem) CloseLogicalChannel(int) error { return nil }
func (readerCapabilitiesModem) DeviceID() string              { return "reader-1" }
func (readerCapabilitiesModem) ExecuteATSilent(string, time.Duration) (string, error) {
	return "", nil
}
func (readerCapabilitiesModem) GetNetworkMode() string                   { return "reader" }
func (readerCapabilitiesModem) GetRegStatus() (int, string)              { return 0, "reader" }
func (readerCapabilitiesModem) IsHealthy() bool                          { return true }
func (readerCapabilitiesModem) IsSimInserted() bool                      { return true }
func (readerCapabilitiesModem) OpenLogicalChannel(string) (int, error)   { return 1, nil }
func (readerCapabilitiesModem) QuerySIMInserted() (bool, error)          { return true, nil }
func (readerCapabilitiesModem) Stop()                                    {}
func (readerCapabilitiesModem) TransmitAPDU(int, string) (string, error) { return "9000", nil }

func (readerCapabilitiesModem) Capabilities() ModemCapabilities {
	return ModemCapabilities{SIM: true, Reader: true, HasUSIM: true}
}

func TestNewModemAccessAdapterPreservesReaderCapabilities(t *testing.T) {
	adapter := NewModemAccessAdapter(readerCapabilitiesModem{})
	capabilities := adapter.Capabilities()
	if !capabilities.Reader || !capabilities.SIM || !capabilities.HasUSIM || capabilities.Modem {
		t.Fatalf("reader capabilities = %+v", capabilities)
	}
}
func TestStartMarksIMSReadyOnlyAfterRegister(t *testing.T) {
	prepared := &identity.PreparedSession{
		Profile: identity.Profile{IMSI: "310260123456789", MCC: "310", MNC: "260"},
		IMSIdentity: identity.IMSIdentity{
			IMPI:   "310260123456789@ims.mnc026.mcc310.3gppnetwork.org",
			IMPU:   "sip:310260123456789@ims.mnc026.mcc310.3gppnetwork.org",
			Domain: "ims.mnc026.mcc310.3gppnetwork.org",
		},
		EPDGAddr: "epdg.example.com",
	}
	req := runtimeTestRequest(prepared, newLifecycleTunnel(nil))
	req.DeviceID = "dev-1"
	req.Prepared = prepared
	inst, err := Start(context.Background(), req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	svc := inst.Service()
	if svc == nil {
		t.Fatal("service not installed")
	}
	// The real service adapter should report the device ID.
	st := svc.Status()
	if st["device_id"] != "dev-1" {
		t.Errorf("device = %v, want dev-1", st["device_id"])
	}
	if !inst.State().IMSReady || !inst.State().SMSReady || inst.State().IMSState != "registered" {
		t.Errorf("IMS state = %+v", inst.State())
	}
}

func TestStartClearsIMSReadyWhenRegistrationRefreshFails(t *testing.T) {
	prepared := &identity.PreparedSession{
		Profile:     identity.Profile{IMSI: "310260123456789", MCC: "310", MNC: "260"},
		IMSIdentity: identity.IMSIdentity{IMPI: "310260123456789@ims.example", IMPU: "sip:310260123456789@ims.example", Domain: "ims.example"},
		EPDGAddr:    "epdg.example.com",
	}
	refreshErrs := make(chan error, 1)
	req := runtimeTestRequest(prepared, newLifecycleTunnel(nil))
	req.IMSFactory = func(req StartRequest, _ Tunnel) (IMSLifecycle, error) {
		return &lifecycleIMS{deviceID: req.DeviceID, refreshErrs: refreshErrs}, nil
	}
	inst, err := Start(context.Background(), req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	refreshErrs <- errors.New("registrar expired")
	deadline := time.After(time.Second)
	for inst.State().IMSReady {
		select {
		case <-deadline:
			t.Fatal("runtime did not clear IMSReady after refresh failure")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	state := inst.State()
	if state.IMSState != "failed" || state.SMSReady || !state.TunnelReady {
		t.Fatalf("runtime refresh failure state = %+v", state)
	}
}

func TestStartRequestsFreshRuntimeForIKEReauthentication(t *testing.T) {
	prepared := &identity.PreparedSession{
		Profile:     identity.Profile{IMSI: "310260123456789", MCC: "310", MNC: "260"},
		IMSIdentity: identity.IMSIdentity{IMPI: "310260123456789@ims.example", IMPU: "sip:310260123456789@ims.example", Domain: "ims.example"},
		EPDGAddr:    "epdg.example.com",
	}
	tunnel := newLifecycleTunnel(nil)
	inst, err := Start(context.Background(), runtimeTestRequest(prepared, tunnel))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	tunnel.fail(swu.ErrFreshRuntimeRequired)

	deadline := time.After(time.Second)
	for inst.State().LastError == "" {
		select {
		case <-deadline:
			t.Fatal("runtime did not expose terminal tunnel failure")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	state := inst.State()
	if state.SessionState != "error" || state.IMSState != "restarting" || state.TunnelReady || state.IMSReady || state.SMSReady {
		t.Fatalf("terminal tunnel state = %+v", state)
	}
	if state.LastErrorClass != ErrorClassReauthentication || state.LastReason != "IKE reauthentication requires fresh runtime" ||
		!strings.Contains(state.LastError, "fresh runtime session") {
		t.Fatalf("terminal tunnel error = %+v", state)
	}
	_ = inst.Stop(context.Background())
}

func TestStartSurfacesEstablishedTunnelFailure(t *testing.T) {
	prepared := &identity.PreparedSession{
		Profile:     identity.Profile{IMSI: "310260123456789", MCC: "310", MNC: "260"},
		IMSIdentity: identity.IMSIdentity{IMPI: "310260123456789@ims.example", IMPU: "sip:310260123456789@ims.example", Domain: "ims.example"},
		EPDGAddr:    "epdg.example.com",
	}
	tunnel := newLifecycleTunnel(nil)
	inst, err := Start(context.Background(), runtimeTestRequest(prepared, tunnel))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	tunnel.fail(errors.New("DPD failed"))

	deadline := time.After(time.Second)
	for inst.State().LastError == "" {
		select {
		case <-deadline:
			t.Fatal("runtime did not expose terminal tunnel failure")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	state := inst.State()
	if state.LastErrorClass != "network" || state.LastReason != "SWu tunnel control failed" ||
		!strings.Contains(state.LastError, "DPD failed") {
		t.Fatalf("terminal tunnel error = %+v", state)
	}
	_ = inst.Stop(context.Background())
}

func TestStartReturnsFailedInstanceWhenTunnelConnectFails(t *testing.T) {
	connectErr := errors.New("IKE_AUTH rejected")
	tunnel := newLifecycleTunnel(connectErr)
	prepared := &identity.PreparedSession{
		Profile:     identity.Profile{IMSI: "310260123456789", MCC: "310", MNC: "260"},
		IMSIdentity: identity.IMSIdentity{IMPI: "310260123456789@ims.example", IMPU: "sip:310260123456789@ims.example", Domain: "ims.example"},
		EPDGAddr:    "epdg.example.com",
	}
	inst, err := Start(context.Background(), runtimeTestRequest(prepared, tunnel))
	if !errors.Is(err, connectErr) {
		t.Fatalf("Start error = %v, want %v", err, connectErr)
	}
	if inst == nil || inst.State().SessionState != "error" || !strings.Contains(inst.State().Error, connectErr.Error()) {
		t.Fatalf("failed instance state = %+v", inst.State())
	}
	select {
	case <-tunnel.done:
	default:
		t.Fatal("failed tunnel was not shut down")
	}
}

func TestStartContextCancellationStopsTunnel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tunnel := newLifecycleTunnel(nil)
	prepared := &identity.PreparedSession{
		Profile:     identity.Profile{IMSI: "310260123456789", MCC: "310", MNC: "260"},
		IMSIdentity: identity.IMSIdentity{IMPI: "310260123456789@ims.example", IMPU: "sip:310260123456789@ims.example", Domain: "ims.example"},
		EPDGAddr:    "epdg.example.com",
	}
	inst, err := Start(ctx, runtimeTestRequest(prepared, tunnel))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	stopped := make(chan struct{}, 1)
	inst.AddObserver(ObserverFunc(func(_ context.Context, event Event) {
		if event.State.SessionState == "stopped" {
			stopped <- struct{}{}
		}
	}))
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not stop runtime")
	}
	select {
	case <-tunnel.done:
	default:
		t.Fatal("context cancellation did not shut down tunnel")
	}
}

func TestStartPassesSOCKS5ProxyToTunnel(t *testing.T) {
	tunnel := newLifecycleTunnel(nil)
	prepared := &identity.PreparedSession{
		Profile:     identity.Profile{IMSI: "310260123456789", MCC: "310", MNC: "260"},
		IMSIdentity: identity.IMSIdentity{IMPI: "310260123456789@ims.example", IMPU: "sip:310260123456789@ims.example", Domain: "ims.example"},
		EPDGAddr:    "epdg.example.com",
	}
	req := runtimeTestRequest(prepared, tunnel)
	req.Proxy = &ProxyConfig{Enabled: true, Addr: "127.0.0.1:1080", Username: "alice", Password: "secret"}
	var captured *swu.Config
	req.TunnelFactory = func(cfg *swu.Config) (Tunnel, error) {
		captured = cfg
		tunnel.onStateChange = cfg.OnStateChange
		return tunnel, nil
	}
	inst, err := Start(context.Background(), req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer inst.Stop(context.Background())
	if captured == nil || captured.ProxyAddr != "127.0.0.1:1080" || captured.Proxy == nil {
		t.Fatalf("captured proxy config = %+v", captured)
	}
	if captured.Proxy.Username != "alice" || captured.Proxy.Password != "secret" {
		t.Fatalf("captured proxy credentials mismatch")
	}
}

func TestStartReturnsFailedInstanceWhenIMSRegisterFails(t *testing.T) {
	registerErr := errors.New("REGISTER rejected with 403")
	tunnel := newLifecycleTunnel(nil)
	prepared := &identity.PreparedSession{
		Profile:     identity.Profile{IMSI: "310260123456789", MCC: "310", MNC: "260"},
		IMSIdentity: identity.IMSIdentity{IMPI: "310260123456789@ims.example", IMPU: "sip:310260123456789@ims.example", Domain: "ims.example"},
		EPDGAddr:    "epdg.example.com",
	}
	req := runtimeTestRequest(prepared, tunnel)
	ims := &lifecycleIMS{deviceID: req.DeviceID, registerErr: registerErr}
	req.IMSFactory = func(StartRequest, Tunnel) (IMSLifecycle, error) { return ims, nil }
	inst, err := Start(context.Background(), req)
	if !errors.Is(err, registerErr) {
		t.Fatalf("Start error = %v, want %v", err, registerErr)
	}
	state := inst.State()
	if state.SessionState != "error" || state.IMSState != "failed" || state.IMSReady || state.TunnelReady {
		t.Fatalf("failed IMS state = %+v", state)
	}
	if !ims.stopped {
		t.Fatal("failed IMS lifecycle was not stopped")
	}
	select {
	case <-tunnel.done:
	default:
		t.Fatal("IMS failure did not close SWu tunnel")
	}
}

func TestStartWaitsForIMSRegistrationBeforeSuccess(t *testing.T) {
	tunnel := newLifecycleTunnel(nil)
	prepared := &identity.PreparedSession{
		Profile:     identity.Profile{IMSI: "310260123456789", MCC: "310", MNC: "260"},
		IMSIdentity: identity.IMSIdentity{IMPI: "310260123456789@ims.example", IMPU: "sip:310260123456789@ims.example", Domain: "ims.example"},
		EPDGAddr:    "epdg.example.com",
	}
	req := runtimeTestRequest(prepared, tunnel)
	ims := &lifecycleIMS{deviceID: req.DeviceID, started: make(chan struct{}), release: make(chan struct{})}
	req.IMSFactory = func(StartRequest, Tunnel) (IMSLifecycle, error) { return ims, nil }
	type startResult struct {
		instance *Instance
		err      error
	}
	result := make(chan startResult, 1)
	go func() {
		instance, err := Start(context.Background(), req)
		result <- startResult{instance: instance, err: err}
	}()
	select {
	case <-ims.started:
	case <-time.After(time.Second):
		t.Fatal("IMS registration was not started")
	}
	select {
	case <-result:
		t.Fatal("Start returned before IMS registration completed")
	default:
	}
	close(ims.release)
	select {
	case got := <-result:
		if got.err != nil || got.instance.State().SessionState != "established" {
			t.Fatalf("Start result = instance %+v error %v", got.instance.State(), got.err)
		}
		_ = got.instance.Stop(context.Background())
	case <-time.After(time.Second):
		t.Fatal("Start did not return after IMS registration")
	}
}

func TestIMSServiceUsesStablePreparedIMEI(t *testing.T) {
	tunnel := newLifecycleTunnel(nil)
	prepared := &identity.PreparedSession{
		Profile: identity.Profile{
			IMSI: "310260123456789", MCC: "310", MNC: "260", IMEI: "860349051234567",
		},
		IMSIdentity: identity.IMSIdentity{IMPI: "310260123456789@ims.example", IMPU: "sip:310260123456789@ims.example", Domain: "ims.example"},
		EPDGAddr:    "epdg.example.com",
	}
	svc, err := imscoreFromPrepared(runtimeTestRequest(prepared, tunnel), tunnel)
	if err != nil {
		t.Fatalf("imscoreFromPrepared: %v", err)
	}
	t.Cleanup(svc.StopCurrent)
	if got := svc.GetIMEI(); got != "860349051234567" {
		t.Fatalf("IMS IMEI = %q, want prepared identity IMEI for sip.instance replacement", got)
	}
}

func TestIMSServiceUsesSWuInnerNetwork(t *testing.T) {
	tunnel := newLifecycleTunnel(nil)
	prepared := &identity.PreparedSession{
		Profile:     identity.Profile{IMSI: "310260123456789", MCC: "310", MNC: "260"},
		IMSIdentity: identity.IMSIdentity{IMPI: "310260123456789@ims.example", IMPU: "sip:310260123456789@ims.example", Domain: "ims.example"},
		EPDGAddr:    "epdg.example.com",
	}
	req := runtimeTestRequest(prepared, tunnel)
	svc, err := imscoreFromPrepared(req, tunnel)
	if err != nil {
		t.Fatalf("imscoreFromPrepared: %v", err)
	}
	t.Cleanup(svc.StopCurrent)
	if got := svc.GetLocalIMSAddr(); got != "10.0.0.2" {
		t.Fatalf("IMS local address = %q, want SWu inner address", got)
	}
	if !svc.IPSec3GPPEnabled() {
		t.Fatal("runtime IMS service did not enable 3GPP IPsec")
	}
}

func TestIMSAPNFromDomain(t *testing.T) {
	if got := imsAPNFromDomain("ims.mnc010.mcc234.3gppnetwork.org"); got != "ims" {
		t.Fatalf("imsAPNFromDomain() = %q, want ims", got)
	}
	if got := imsAPNFromDomain("   "); got != "" {
		t.Fatalf("empty imsAPNFromDomain() = %q", got)
	}
}

func TestCustomTunnelIMSServiceUsesHostSubscriptionRetries(t *testing.T) {
	tunnel := newLifecycleTunnel(nil)
	prepared := &identity.PreparedSession{
		Profile:     identity.Profile{IMSI: "310260123456789", MCC: "310", MNC: "260"},
		IMSIdentity: identity.IMSIdentity{IMPI: "310260123456789@ims.example", IMPU: "sip:310260123456789@ims.example", Domain: "ims.example"},
		EPDGAddr:    "epdg.example.com",
	}
	req := runtimeTestRequest(prepared, tunnel)
	req.SubscriptionRetries = NewSubscriptionRetries()
	for range 2 {
		svc, err := imscoreFromPrepared(req, tunnel)
		if err != nil {
			t.Fatalf("imscoreFromPrepared: %v", err)
		}
		svc.StopCurrent()
		if svc.SubscriptionRetries() != req.SubscriptionRetries {
			t.Fatal("custom tunnel IMS service did not use the host subscription retry store")
		}
	}
}

type wakeableLifecycleIMS struct {
	lifecycleIMS
	wakes int
}

func (s *wakeableLifecycleIMS) WakeMWISubscription() bool {
	s.wakes++
	return true
}

func TestInstanceWakeMWISubscriptionReachesTheIMSService(t *testing.T) {
	var none *Instance
	if none.WakeMWISubscription() || (&Instance{}).WakeMWISubscription() {
		t.Fatal("wake without an IMS service reported success")
	}
	// Custom-tunnel path with an injected lifecycle.
	ims := &wakeableLifecycleIMS{}
	inst := &Instance{}
	inst.setService(lifecycleServiceAdapter{lifecycle: ims})
	if !inst.WakeMWISubscription() || ims.wakes != 1 {
		t.Fatalf("custom tunnel wake reached the lifecycle %d times", ims.wakes)
	}
	inst.setService(lifecycleServiceAdapter{lifecycle: &lifecycleIMS{}})
	if inst.WakeMWISubscription() {
		t.Fatal("a lifecycle without MWI reported a wake")
	}
	// Default path and the built-in tunnel IMS reach a real imscore Service.
	tunnel := newLifecycleTunnel(nil)
	prepared := &identity.PreparedSession{
		Profile:     identity.Profile{IMSI: "310260123456789", MCC: "310", MNC: "260"},
		IMSIdentity: identity.IMSIdentity{IMPI: "310260123456789@ims.example", IMPU: "sip:310260123456789@ims.example", Domain: "ims.example"},
		EPDGAddr:    "epdg.example.com",
	}
	svc, err := imscoreFromPrepared(runtimeTestRequest(prepared, tunnel), tunnel)
	if err != nil {
		t.Fatalf("imscoreFromPrepared: %v", err)
	}
	t.Cleanup(svc.StopCurrent)
	for _, service := range []Service{
		newServiceAdapter(svc),
		lifecycleServiceAdapter{lifecycle: &imscoreLifecycleAdapter{svc: svc}},
	} {
		inst.setService(service)
		if !inst.WakeMWISubscription() {
			t.Fatalf("%T wake did not reach the imscore Service", service)
		}
	}
	for _, service := range []Service{
		newServiceAdapter(nil),
		lifecycleServiceAdapter{lifecycle: &imscoreLifecycleAdapter{}},
	} {
		inst.setService(service)
		if inst.WakeMWISubscription() {
			t.Fatalf("%T without an imscore Service reported a wake", service)
		}
	}
}
