package swu

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iniwex5/vowifi-go/engine/crypto"
	"github.com/iniwex5/vowifi-go/engine/driver"
	engineeap "github.com/iniwex5/vowifi-go/engine/eap"
	"github.com/iniwex5/vowifi-go/engine/ikev2"
	"github.com/iniwex5/vowifi-go/engine/ipsec"
	"github.com/iniwex5/vowifi-go/engine/logger"
	enginesim "github.com/iniwex5/vowifi-go/engine/sim"
	"github.com/iniwex5/vowifi-go/engine/swu/eapaka"
	"go.uber.org/zap"
)

// ErrFreshRuntimeRequired tells the host to replace the current IKE runtime.
var ErrFreshRuntimeRequired = errors.New("swu: full reauthentication requires a fresh runtime session")

// Transport is the original injectable SWu network boundary.
type Transport = ipsec.Transport

// TUN is the original injectable inner-packet device boundary.
type TUN interface {
	Close() error
	DeviceName() string
	Read([]byte) (int, error)
	Write([]byte) (int, error)
}

// NetTools is the original injectable network-configuration boundary.
type NetTools interface {
	AddAddress(string, string) error
	AddAddress6(string, string) error
	AddRoute(string, string, string) error
	AddRoute6(string, string, string) error
	SetLinkUp(string) error
	SetMTU(string, int) error
}

// Config carries the SWu session configuration recovered from the decompiled
// engine/swu. It is the input to NewSession.
type Config struct {
	// DeviceID identifies the access device in transport diagnostics.
	DeviceID string
	// DNSServer optionally selects the resolver used for ePDG lookup.
	DNSServer string
	// EPDGCandidates optionally rotates SOCKS5/injected transport candidates after address rejection.
	// The runtime owns its lifetime; nil retains legacy endpoint selection.
	EPDGCandidates *EPDGCandidateStore
	// EPDGAddr is the ePDG host (FQDN or IP) and optional port.
	EPDGAddr string
	// EpDGAddr/EpDGPort are the original endpoint fields.
	EpDGAddr string
	EpDGPort uint16
	// APN is carried as IDr in the first IKE_AUTH request (3GPP TS 24.302).
	// Empty requests the operator's default APN.
	APN string
	// LocalIP is the local address to bind the IKE/ESP socket to.
	LocalIP net.IP
	// LocalAddr/LocalPort retain the original bind-address API.
	LocalAddr string
	LocalPort uint16
	// ProxyAddr and Proxy route IKE/ESP through a SOCKS5 UDP associate.
	ProxyAddr string
	Proxy     *ipsec.Socks5Config
	// Socks5Addr/Socks5Username/Socks5Password are the original proxy fields.
	Socks5Addr     string
	Socks5Username string
	Socks5Password string
	// TransportFactory restores the original injectable transport constructor.
	TransportFactory func(local, remote string) (Transport, error)
	// TUNFactory and NetTools restore the original injectable driver boundaries.
	TUNFactory func(name string) (TUN, error)
	NetTools   NetTools
	// IMSI is the subscriber IMSI used for the EAP-AKA identity.
	IMSI string
	// MCC/MNC override the MCC/MNC derived from the IMSI in the NAI.
	MCC string
	MNC string
	// AKAProvider computes AKA from the network challenge (RAND, AUTN).
	AKAProvider AKAProvider
	// SIM and IPStackType retain the original configuration fields. AKAProvider
	// and IPStack are current aliases kept for source compatibility.
	SIM         AKAProvider
	IPStackType string
	IPStack     string
	// The following fields retain the original IKE_AUTH/EAP identity and
	// interoperability policy. DisableEAPMACValidation is an explicit unsafe
	// diagnostic switch and is never enabled by default.
	DisableEAPMACValidation bool
	// VerifyFinalResponderAUTH enables the post-EAP MSK proof check added by
	// the rewrite. The original engine did not enforce that final proof.
	VerifyFinalResponderAUTH  bool
	EnableDeviceIdentitySpoof bool
	DeviceIdentityIMEI        string
	WithholdDeviceIdentity    bool // keep DEVICE_IDENTITY out of the first IKE_AUTH
	KeepChildSAOnRekeyDecline bool // keep the CHILD_SA when its rekey gets NO_ADDITIONAL_SAS
	DeferPeerChildRekeySwitch bool // after answering a peer CHILD_SA rekey, send on the old SA until the peer uses the new one
	IKEIdentityMode           string
	AKAChallengeMode          string
	AKAIdentityMode           string
	AKAPrimePreferred         bool
	// Fast reauthentication material can be restored by the runtime host.
	FastReauthID    string
	FastReauthMK    []byte
	FastReauthKAut  []byte
	FastReauthKEncr []byte
	// OnFastReauthUpdate persists a newly issued reauthentication identity.
	OnFastReauthUpdate func(reauthID string, mk, kAut, kEncr []byte)
	// OmitInitialContact skips the IKE_AUTH INITIAL_CONTACT notify. RFC 7296
	// 2.8.3 requires omitting it while an older IKE SA is still forwarding.
	OmitInitialContact bool
	// ResumeTicket and ResumeOldSKd restore the RFC 5723 cross-session
	// credential. OnTicketUpdate persists replacement or invalidation.
	ResumeTicket   []byte
	ResumeOldSKd   []byte
	OnTicketUpdate func(ticket, skd []byte)
	// AlgorithmPolicy selects the IKE/ESP algorithm offer policy.
	AlgorithmPolicy string
	// IKEProposals and ESPProposals are ordered legacy proposal strings. Empty
	// selects the original multi-proposal compatibility set.
	IKEProposals []string
	ESPProposals []string
	// Legacy encryption is disabled unless explicitly enabled and allowed.
	EnableLegacyCiphers  bool
	AllowedLegacyCiphers []string
	// IKEEncryption / IKEPRF / IKEIntegrity / IKEDH are the IKE algorithm
	// transform IDs (RFC 7296 §3.3.2). Zero selects the policy default.
	IKEEncryption        uint16
	IKEEncryptionKeyBits uint16
	IKEPRF               uint16
	IKEIntegrity         uint16
	IKEDH                uint16
	// ESPEncryption / ESPIntegrity are the ESP transform IDs for the CHILD_SA.
	ESPEncryption        uint16
	ESPEncryptionKeyBits uint16
	ESPIntegrity         uint16
	// EnableDriver restores the original empty-mode TUN switch. An explicit
	// DataplaneMode takes precedence so current callers retain their behavior.
	EnableDriver bool
	// DataplaneMode selects userspace, TUN, or Linux XFRM processing.
	DataplaneMode string
	TUNName       string
	TUNMTU        int
	XFRMIfID      uint32
	ReplayWindow  int
	EnableESN     bool
	// NonceLen is the initiator nonce length (default 32).
	NonceLen int
	// RekeyIKESeconds / RekeyChildSeconds drive the SA rekey timers.
	RekeyIKESeconds   time.Duration
	RekeyChildSeconds time.Duration
	ReauthSeconds     time.Duration
	NATKeepaliveEvery time.Duration
	DPDProbeEvery     time.Duration
	// Original timer fields are expressed in seconds. The duration aliases take
	// precedence when both forms are configured.
	ReauthInterval      int
	NATKeepaliveSeconds int
	DPDIntervalSeconds  int
	// Retransmit controls IKE request retries. Nil selects the recovered
	// RFC 7296 policy used by TaskManager.
	Retransmit *RetransmitConfig
	// IKERetryConfig is the original sliding-window retry configuration.
	IKERetryConfig *RetryConfig
	// Wireshark enables the pcap-like traffic logger.
	Wireshark bool
	// Original Wireshark key-log configuration.
	EnableWiresharkKeyLog bool
	WiresharkKeyLogPath   string
	// OnProgress reports the original user-visible connection milestones.
	OnProgress func(string)
	// OnRedirect is invoked when the ePDG redirects the session (RFC 5685).
	OnRedirect func(target string)
	// OnStateChange is invoked on session state transitions.
	OnStateChange func(state string)
}

// AKAProvider computes AKA from the network challenge (RAND, AUTN).
type AKAProvider = enginesim.AKAProvider

// AKAResult is the outcome of an AKA computation.
type AKAResult = enginesim.AKAResult

// Session state strings (recovered from the decompiled status.go).
const (
	stateIdle           = "idle"
	stateConnecting     = "connecting"
	stateAuthenticating = "authenticating"
	stateEstablished    = "established"
	stateError          = "error"
	stateShutdown       = "shutdown"
)

// Data-plane modes recovered from the original session configuration.
const (
	DataplaneModeUserspace = "userspace"
	DataplaneModeTUN       = "tun"
	DataplaneModeXFRMI     = "xfrmi"
)

// ikeAuthStage tracks the IKE_AUTH exchange progress.
type ikeAuthStage int

const (
	stageInit  ikeAuthStage = iota // build & send IKE_AUTH request
	stageEAP                       // EAP exchange in progress
	stageFinal                     // final IKE_AUTH request (AUTH + EAP success)
	stageDone                      // IKE_AUTH complete
)

// Session is the SWu IKEv2 + EAP-AKA session. It extends the key-derivation
// fields in types.go with the transport, IKE_AUTH state, data plane and timers
// recovered from the decompiled engine/swu.
type Session struct {
	// --- IKE identifiers / negotiation (types.go) ---
	spiI [8]byte
	spiR [8]byte
	// Original public session fields. Internal packet code retains byte-array
	// SPIs and synchronizes these numeric projections at every SA transition.
	SPIi              uint64
	SPIr              uint64
	EncAlg            crypto.Encrypter
	IntegAlg          crypto.IntegrityAlgorithm
	PRFAlg            crypto.PRF
	DH                *crypto.DiffieHellman
	Keys              *ikev2.IKESAKeys
	SequenceNumber    atomic.Uint32
	localIKEInitiator bool
	Ni                []byte
	nr                []byte // responder nonce (stored during IKE_SA_INIT)

	prf    crypto.PRF
	prfKey []byte

	integKeyLen    int
	encKeyLen      int
	encKeyBits     uint16
	aead           bool
	espEncKeyLen   int
	espEncKeyBits  uint16
	espIntegKeyLen int
	espAEAD        bool

	dhSharedSecret   []byte
	ikeKeys          *IKEKeys
	retiredIKESA     *ikeSAContext
	retiredIKEDelete *retiredIKEDeleteReceipt

	dh       *crypto.DiffieHellman
	dhGroup  uint16
	encrAlg  uint16
	prfAlg   uint16
	integAlg uint16
	nonceLen int

	cookie []byte

	natSourceHash []byte
	natDestHash   []byte
	natDetected   bool

	// --- configuration ---
	cfg *Config

	// --- transport ---
	transportMu   sync.RWMutex
	socket        ipsec.Transport
	epdgCandidate *epdgCandidateAttempt

	// --- IKE_AUTH state ---
	stage                     ikeAuthStage
	eapID                     byte // current EAP identifier
	eapType                   byte // negotiated EAP method (AKA / AKA')
	eapKeys                   eapaka.Keys
	fastReauthCtx             *engineeap.FastReauthContext
	ikeIdentity               string
	eapIdentity               string
	eapIdentitySet            bool
	eapTranscript             [][]byte
	eapIdentityTranscript     [][]byte
	eapResultIndicated        bool
	eapResultConfirmed        bool
	eapSuccessReceived        bool
	deviceIdentityRequested   bool // owned by the serial IKE_AUTH loop
	deviceIdentityEAPVerified bool
	authPayload               []byte // responder AUTH payload (for verification)
	skf                       []byte // SKF (encrypted IKE_AUTH response) pending decrypt
	responderAuthenticated    bool
	eapOnlyAuthentication     bool
	eapOnlyRequested          bool
	responderIDType           byte
	responderID               []byte
	ikeSAInitRequest          []byte
	ikeSAInitResponse         []byte

	// --- data plane ---
	innerEndpointMu         sync.RWMutex
	innerEndpoint           *userspaceInnerPacketEndpoint
	dataPlaneHandleMu       sync.RWMutex
	tun                     TUN
	net                     NetTools
	networkTxn              *driver.NetTxn
	legacyNetwork           *legacyNetTxn
	kernelDataPlane         kernelDataPlane
	xfrmManagerNew          func() xfrmManager
	espOutboundSA           *ipsec.SecurityAssociation
	espInboundSA            *ipsec.SecurityAssociation
	espInboundSAs           map[uint32]*ipsec.SecurityAssociation
	ChildSAIn               *ipsec.SecurityAssociation
	ChildSAOut              *ipsec.SecurityAssociation
	ChildSAsIn              map[uint32]*ipsec.SecurityAssociation
	retiredChildSAs         map[uint32]uint32
	espLocalSPI             uint32
	espRemoteSPI            uint32
	childNi                 []byte
	childNr                 []byte
	childDH                 *crypto.DiffieHellman
	childDHSecret           []byte
	pendingPeerRekey        *pendingPeerChildRekey
	pendingRekeySPI         atomic.Uint32 // inbound SPI of pendingPeerRekey for the data path
	childRekeyOmitDH        atomic.Bool   // the last no-PFS CHILD_SA rekey that worked left D-H out
	lastCreateChildSAAnswer *createChildSAAnswer
	childTSi                *ikev2.EncryptedPayloadTS
	childTSr                *ikev2.EncryptedPayloadTS
	espCipher               uint16
	espInteg                uint16
	espESN                  bool
	espKey                  []byte
	espIntegKey             []byte
	innerIP                 net.IP // inner IP assigned by the ePDG (CP payload)
	innerIPv6               net.IP
	innerPrefix             int
	innerIPv6Prefix         int
	dnsServers              []net.IP
	pcscfServers            []net.IP
	remoteIP                net.IP // ePDG outer address
	remotePort              int

	// --- lifecycle ---
	ctx             context.Context
	cancel          context.CancelFunc
	done            chan struct{}
	doneOnce        sync.Once
	sessionDownOnce sync.Once
	cleanupOnce     sync.Once
	deleteOnce      sync.Once

	mu                sync.RWMutex
	childSAMu         sync.RWMutex
	ikeExchangeMu     sync.Mutex
	rekeyMu           sync.Mutex
	controlMu         sync.RWMutex
	controlWG         sync.WaitGroup
	controlRequests   chan []byte
	controlStop       chan struct{}
	controlTransport  ipsec.Transport
	controlRunning    bool
	controlStopping   bool
	taskMgr           *TaskManager
	ikeWaiters        map[ikeWaitKey]chan []byte
	ikePending        map[ikeWaitKey][]byte
	terminalErr       error
	cleanupErr        error // local cleanup failures only, see CleanupError
	initErr           error
	state             string
	dataPlaneStarted  bool
	tunStats          dataPlaneRuntimeStats
	dataPlaneWG       sync.WaitGroup
	sessionStatsOnce  sync.Once
	sessionStatsWG    sync.WaitGroup
	rekeyTimerWG      sync.WaitGroup
	netEventWG        sync.WaitGroup
	netEventMu        sync.Mutex
	netEventMonitors  map[<-chan ipsec.NetEvent]struct{}
	netEventClosing   bool
	startedAt         time.Time
	lastPingAt        time.Time
	lastDPDAt         time.Time
	activityMu        sync.RWMutex
	lastInboundTime   time.Time
	lastOutboundTime  time.Time
	rekeyResetCh      chan struct{}
	childRekeyResetCh chan struct{}
	lastRekeyTime     time.Time
	lastIKERekeyTime  time.Time
	authLifetime      uint32
	lastIKERequest    []byte
	lastIKERequestSet [][]byte
	lastIKEResponse   []byte
	nextOutboundID    uint32

	// --- timers ---
	timersMu            sync.Mutex
	ikeReauthTimer      *time.Timer
	ikeRekeyTimer       *time.Timer
	childRekeyTimer     *time.Timer
	natKeepalive        *time.Timer
	dpdTimer            *time.Timer
	natKeepaliveStarted bool

	// --- wireshark ---
	debugMu sync.Mutex
	debug   *WiresharkDebugger
	Logger  *zap.Logger

	// --- IKE_SA_INIT proposal negotiation ---
	ikeProfileOffset         int
	offeredIKEProfiles       []string
	offeredIKEProposals      []*ikev2.Proposal
	offeredESPProposals      []*ikev2.Proposal
	effectiveCipherPolicy    string
	negotiationFallbackCount int
	sendCookie               bool
	fragmentationSupported   bool
	mobikeSupported          bool
	sessionResumed           bool
	resumeTicket             []byte
	resumeOldSKd             []byte
	ikeFragmentMTU           uint32
	fragmentBuf              *fragmentBuffer

	// Original session lifecycle callbacks. OnSessionDown is used by XFRM
	// hard-expire handling; OnReauthNeeded is consumed by the lifecycle stage.
	OnSessionDown      func()
	OnReauthNeeded     func()
	OnRedirect         func(string)
	reauthOverlapGrace time.Duration
	xfrmRekey          func() error
	xfrmActionMu       sync.Mutex
	xfrmActionWG       sync.WaitGroup
	xfrmActionClosing  bool
}

// NewSession builds a SWu session from the configuration.
func NewSession(cfg *Config, loggers ...*zap.Logger) *Session {
	if cfg == nil {
		cfg = &Config{}
	}
	sessionLogger := logger.L()
	if len(loggers) > 0 && loggers[0] != nil {
		sessionLogger = loggers[0]
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		cfg:               cfg,
		ctx:               ctx,
		cancel:            cancel,
		done:              make(chan struct{}),
		state:             stateIdle,
		startedAt:         time.Now(),
		rekeyResetCh:      make(chan struct{}, 1),
		childRekeyResetCh: make(chan struct{}, 1),
		ikeWaiters:        make(map[ikeWaitKey]chan []byte),
		ikePending:        make(map[ikeWaitKey][]byte),
		nonceLen:          cfg.NonceLen,
		nextOutboundID:    1,
		localIKEInitiator: true,
		fastReauthCtx:     initFastReauthContext(cfg),
		fragmentBuf:       newFragmentBuffer(),
		ikeFragmentMTU:    defaultFragmentMTU,
		espInboundSAs:     make(map[uint32]*ipsec.SecurityAssociation),
		ChildSAsIn:        make(map[uint32]*ipsec.SecurityAssociation),
		retiredChildSAs:   make(map[uint32]uint32),
		netEventMonitors:  make(map[<-chan ipsec.NetEvent]struct{}),
		resumeTicket:      append([]byte(nil), cfg.ResumeTicket...),
		resumeOldSKd:      append([]byte(nil), cfg.ResumeOldSKd...),
		net:               configuredNetTools(cfg),
		xfrmManagerNew:    func() xfrmManager { return driver.NewXFRMManager() },
		Logger:            sessionLogger,
		OnRedirect:        cfg.OnRedirect,
	}
	if s.nonceLen <= 0 {
		s.nonceLen = 32
	}
	s.initErr = initializeSessionAlgorithms(s, cfg)
	s.syncLegacyIKEStateLocked()
	s.SequenceNumber.Store(0)
	s.syncLegacyChildStateLocked()
	return s
}

func initFastReauthContext(cfg *Config) *engineeap.FastReauthContext {
	context := engineeap.NewFastReauthContext()
	if cfg != nil && cfg.FastReauthID != "" && len(cfg.FastReauthMK) > 0 {
		context.SaveReauthData(cfg.FastReauthID, cfg.FastReauthMK, cfg.FastReauthKEncr, cfg.FastReauthKAut)
	}
	return context
}

// setState records a session state transition and fires the callback.
func (s *Session) setState(st string) {
	s.mu.Lock()
	s.state = st
	s.mu.Unlock()
	if s.cfg != nil && s.cfg.OnStateChange != nil {
		s.cfg.OnStateChange(st)
	}
}

// State returns the current session state string.
func (s *Session) State() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// setTerminalError records the terminal error and transitions to error state.
func (s *Session) setTerminalError(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	if s.terminalErr == nil {
		s.terminalErr = err
	}
	s.mu.Unlock()
	s.setState(stateError)
}

// TerminalError returns the error that ended an established session, if any.
func (s *Session) TerminalError() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.terminalErr
}

func (s *Session) signalDone() {
	s.doneOnce.Do(func() { close(s.done) })
}

// Connect establishes the SWu tunnel: IKE_SA_INIT → IKE_AUTH (EAP-AKA) →
// CREATE_CHILD_SA. It retries on redirect (RFC 5685) and returns once the data
// plane is up or the session fails terminally.
func (s *Session) Connect(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.bindConnectContext(ctx); err != nil {
		return err
	}
	if s.initErr != nil {
		err := fmt.Errorf("swu: initialize session: %w", s.initErr)
		s.finishConnectFailure(err)
		return err
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		err := s.connectOnce(ctx)
		if err == nil {
			go s.watchConnectContext()
			return nil
		}
		var redir *RedirectError
		if errors.As(err, &redir) {
			target := redir.NewAddr
			if target == "" {
				target = redir.Target
			}
			if s.OnRedirect != nil {
				s.OnRedirect(target)
			}
			s.cfg.EPDGAddr, s.cfg.EpDGAddr = target, target
			s.resetForRedirect()
			lastErr = err
			continue
		}
		s.finishConnectFailure(err)
		return err
	}
	if lastErr != nil {
		s.finishConnectFailure(lastErr)
		return lastErr
	}
	err := errors.New("swu: connect failed")
	s.finishConnectFailure(err)
	return err
}

func (s *Session) bindConnectContext(parent context.Context) error {
	nextContext, nextCancel := context.WithCancel(parent)
	s.mu.Lock()
	if s.state != stateIdle {
		state := s.state
		s.mu.Unlock()
		nextCancel()
		if err := parent.Err(); err != nil {
			return err
		}
		return fmt.Errorf("swu: cannot connect session in %s state", state)
	}
	previousCancel := s.cancel
	s.ctx, s.cancel, s.state = nextContext, nextCancel, stateConnecting
	s.mu.Unlock()
	previousCancel()
	if s.cfg != nil && s.cfg.OnStateChange != nil {
		s.cfg.OnStateChange(stateConnecting)
	}
	return nil
}

func (s *Session) finishConnectFailure(err error) {
	s.setTerminalError(err)
	s.cancel()
	preserveResume := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	var rejection *IKEAuthError
	if errors.As(err, &rejection) && rejection.NotifyType == ikev2.INTERNAL_ADDRESS_FAILURE {
		// A cleanup timeout does not turn a rejected handshake into a resumable one.
		preserveResume = false
	}
	s.cleanupResources(preserveResume)
}

func (s *Session) failSession(err error) {
	if err == nil {
		return
	}
	s.mu.RLock()
	wasEstablished := s.state == stateEstablished
	s.mu.RUnlock()
	if wasEstablished && s.Logger != nil {
		// OnSessionDown only reports "swu_session_down"; keep the cause.
		s.Logger.Warn("SWu established session failed", zap.Error(err))
	}
	s.sendEstablishedDeletes()
	s.setTerminalError(err)
	if wasEstablished {
		s.notifySessionDown()
	}
	s.cancel()
	go s.cleanupResources(false)
}

func (s *Session) notifySessionDown() {
	if s == nil || s.OnSessionDown == nil {
		return
	}
	s.sessionDownOnce.Do(func() { go s.OnSessionDown() })
}

func (s *Session) watchConnectContext() {
	<-s.ctx.Done()
	if s.TerminalError() != nil {
		s.cleanupResources(false)
		return
	}
	s.Shutdown()
}

func (s *Session) resetForRedirect() {
	s.clearIKEKeyMaterial()
	s.spiI, s.spiR = [8]byte{}, [8]byte{}
	s.Ni, s.nr, s.cookie = nil, nil, nil
	s.natDetected = false
	s.sendCookie = false
	s.ikeProfileOffset = 0
	s.syncLegacyIKEStateLocked()
}

// connectOnce runs a single IKE_SA_INIT → IKE_AUTH → CREATE_CHILD_SA attempt.
func (s *Session) connectOnce(ctx context.Context) (err error) {
	if s.cfg == nil {
		return errors.New("swu: no configuration")
	}
	if configuredEPDGAddress(s.cfg) == "" {
		return errors.New("swu: no ePDG address configured")
	}
	defer func() {
		s.finishEPDGCandidate(err)
		if err != nil {
			err = s.cleanupConnectAttempt(ctx, err)
		}
	}()

	// Resolve the ePDG and build the IKE/ESP transport.
	if err := s.buildTransport(ctx); err != nil {
		return fmt.Errorf("build transport: %w", err)
	}
	s.startSessionStats()
	s.startNetEventMonitor()
	resumed, resumeErr := s.trySessionResumption(ctx)
	if resumeErr != nil && (ctx.Err() != nil || s.ctx.Err() != nil) {
		return resumeErr
	}
	if !resumed {
		if err := s.runIKESAInit(ctx); err != nil {
			return joinSessionResumeFallbackError(resumeErr, err)
		}
		if err := s.runIKEAuthLoop(ctx); err != nil {
			return joinSessionResumeFallbackError(resumeErr, err)
		}
	}

	// Some ePDGs establish the first CHILD_SA in IKE_AUTH. If the responder did
	// not return SAr2, create it explicitly with a CREATE_CHILD_SA exchange.
	if s.espRemoteSPI == 0 {
		if err := s.createInitialChildSA(ctx); err != nil {
			return err
		}
	}

	// Bring up the data plane.
	if err := s.setupDataPlane(); err != nil {
		return fmt.Errorf("setup data plane: %w", err)
	}
	if err := s.startEstablishedDataPlane(); err != nil {
		return fmt.Errorf("start data plane: %w", err)
	}
	if err := s.startIKEControl(); err != nil {
		return fmt.Errorf("start IKE control plane: %w", err)
	}
	s.setState(stateEstablished)
	s.startTimers()
	return nil
}

func (s *Session) stopTransport() {
	transport := s.takeTransport()
	if transport == nil {
		return
	}
	transport.Stop()
}

// runIKESAInit performs the IKE_SA_INIT exchange with COOKIE / INVALID_KE /
// REDIRECT handling.
func (s *Session) runIKESAInit(ctx context.Context) error {
	for {
		raw, err := s.buildIKESAInitPacket()
		if err != nil {
			return err
		}
		if err := s.sendIKE(raw); err != nil {
			return fmt.Errorf("send IKE_SA_INIT: %w", err)
		}
		resp, err := s.receiveIKE(ctx)
		if err != nil {
			return fmt.Errorf("receive IKE_SA_INIT response: %w", err)
		}
		responseData, err := resp.Encode()
		if err != nil {
			return fmt.Errorf("encode received IKE_SA_INIT response: %w", err)
		}
		err = s.handleIKESAInitResp(responseData)
		if err == nil {
			s.mu.Lock()
			s.ikeSAInitRequest = append([]byte(nil), s.lastIKERequest...)
			s.ikeSAInitResponse = append([]byte(nil), s.lastIKEResponse...)
			s.mu.Unlock()
			return nil
		}
		if errors.Is(err, errCookieRequired) {
			continue // resend with the cookie
		}
		var negotiationError *NegotiationError
		if errors.As(err, &negotiationError) && negotiationError.Retryable && s.advanceIKEProfileOffset() {
			s.resetIKEInitMaterial()
			continue
		}
		var groupError *ErrInvalidKEGroup
		if errors.As(err, &groupError) {
			if err := s.selectRequestedDHGroup(groupError); err != nil {
				return err
			}
			continue
		}
		return err
	}
}

// buildTransport resolves the ePDG and opens the IKE/ESP socket.
func (s *Session) buildTransport(ctx context.Context) error {
	endpoint := configuredEPDGAddress(s.cfg)
	host, port := endpoint, "500"
	if h, p, err := net.SplitHostPort(endpoint); err == nil {
		host, port = h, p
	} else if s.cfg.EpDGPort != 0 {
		port = fmt.Sprintf("%d", s.cfg.EpDGPort)
	}
	target, err := s.selectEPDGTransportEndpoint(ctx, net.JoinHostPort(host, port))
	if err != nil {
		return err
	}
	host, port, err = net.SplitHostPort(target)
	if err != nil {
		return err
	}
	if s.cfg.TransportFactory != nil {
		localIP := configuredLocalIP(s.cfg)
		localHost := ""
		if localIP != nil {
			localHost = localIP.String()
		}
		localAddr := net.JoinHostPort(localHost, fmt.Sprintf("%d", s.cfg.LocalPort))
		targetAddr := net.JoinHostPort(host, port)
		transport, err := s.cfg.TransportFactory(localAddr, targetAddr)
		if err != nil {
			return fmt.Errorf("open injected IKE transport: %w", err)
		}
		if transport == nil {
			return errors.New("swu: transport factory returned nil")
		}
		if err := transport.Start(); err != nil {
			transport.Stop()
			return fmt.Errorf("start injected IKE transport: %w", err)
		}
		s.setTransport(transport)
		s.remoteIP, s.remotePort = transport.RemoteIP(), transport.RemotePort()
		return nil
	}
	if strings.TrimSpace(configuredProxyAddress(s.cfg)) != "" {
		return s.buildProxyTransport(host, port)
	}
	localIP := configuredLocalIP(s.cfg)
	if localIP == nil {
		ip, err := detectOutboundIPv4ByHost(host, port)
		if err != nil {
			return fmt.Errorf("detect outbound IP: %w", err)
		}
		localIP = ip
	}
	localAddr := net.JoinHostPort(localIP.String(), "0")
	targetAddr := net.JoinHostPort(host, port)
	sm, err := ipsec.NewSocketManager(s.cfg.DeviceID, localAddr, targetAddr, s.cfg.DNSServer)
	if err != nil {
		return fmt.Errorf("open IKE socket: %w", err)
	}
	if err := sm.Start(); err != nil {
		sm.Stop()
		return fmt.Errorf("start IKE socket: %w", err)
	}
	s.setTransport(sm)
	s.remoteIP = sm.RemoteIP()
	s.remotePort = sm.RemotePort()
	return nil
}

func (s *Session) proxyTransportConfig(remote string) ipsec.Socks5Config {
	proxyCfg := ipsec.Socks5Config{}
	if s.cfg.Proxy != nil {
		proxyCfg = *s.cfg.Proxy
	}
	proxyCfg.ProxyAddr, proxyCfg.RemoteAddr = configuredProxyAddress(s.cfg), remote
	proxyCfg.Username, proxyCfg.Password = configuredProxyCredentials(s.cfg)
	proxyCfg.DNSServer, proxyCfg.DeviceID = s.cfg.DNSServer, s.cfg.DeviceID
	return proxyCfg
}

func (s *Session) buildProxyTransport(host, port string) error {
	transport, err := ipsec.NewSocks5Transport(s.proxyTransportConfig(net.JoinHostPort(host, port)))
	if err != nil {
		return fmt.Errorf("open SOCKS5 IKE transport: %w", err)
	}
	if err := transport.Start(); err != nil {
		transport.Stop()
		return fmt.Errorf("start SOCKS5 IKE transport: %w", err)
	}
	s.setTransport(transport)
	s.remoteIP = transport.RemoteIP()
	s.remotePort = transport.RemotePort()
	return nil
}

func configuredEPDGAddress(cfg *Config) string {
	if cfg.EPDGAddr != "" {
		return cfg.EPDGAddr
	}
	return cfg.EpDGAddr
}

func configuredLocalIP(cfg *Config) net.IP {
	if cfg.LocalIP != nil {
		return cfg.LocalIP
	}
	return net.ParseIP(cfg.LocalAddr)
}

// detectOutboundIPv4ByHost resolves a configured hostname before invoking the
// original IP-based outbound-route detector.
func detectOutboundIPv4ByHost(host, port string) (net.IP, error) {
	remoteIP, err := resolveEPDGAddress(host)
	if err != nil {
		return nil, err
	}
	remotePort, err := net.LookupPort("udp", port)
	if err != nil {
		return nil, err
	}
	return detectOutboundIPv4(remoteIP, uint16(remotePort))
}

// detectOutboundRoute resolves the ePDG host to an IP (used by buildTransport).
func resolveEPDGAddress(host string) (net.IP, error) {
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if ip.To4() != nil {
			return ip, nil
		}
	}
	if len(ips) > 0 {
		return ips[0], nil
	}
	return nil, errors.New("swu: no address for ePDG")
}

// Shutdown tears down the session: stops timers, closes the transport and the
// data plane, and marks the session done.
func (s *Session) Shutdown() {
	s.mu.Lock()
	if s.state == stateShutdown {
		s.mu.Unlock()
		return
	}
	s.state = stateShutdown
	s.mu.Unlock()

	s.sendEstablishedDeletes()
	s.cleanupResources(false)
	s.clearSessionResumptionMemory()
}

// sendEstablishedDeletes emits Delete CHILD_SA then Delete IKE_SA while the
// transport and keys are still valid. Failures are logged and do not block
// cleanup. Unestablished sessions skip the exchange.
func (s *Session) sendEstablishedDeletes() {
	if s == nil {
		return
	}
	s.deleteOnce.Do(func() {
		if s.transport() == nil || s.ikeKeys == nil {
			return
		}
		if spi := s.espLocalSPI; spi != 0 {
			if err := s.sendDeleteChildSA([]uint32{spi}); err != nil {
				s.Logger.Warn("send CHILD_SA Delete before shutdown", zap.Error(err))
			}
		}
		if err := s.sendDeleteIKE(); err != nil {
			s.Logger.Warn("send IKE_SA Delete before shutdown", zap.Error(err))
		}
	})
}

func (s *Session) cleanupResources(preserveResume bool) {
	s.cleanupOnce.Do(func() {
		s.cancel()
		s.closeNetEventLifecycle()
		s.stopTimers()
		if s.fragmentBuf != nil {
			s.fragmentBuf.clear()
		}
		cleanupErr := errors.Join(s.stopDataPlane(), s.stopIKEControl())
		s.sessionStatsWG.Wait()
		s.rekeyTimerWG.Wait()
		s.netEventWG.Wait()
		s.stopTransport()
		s.clearIKEKeyMaterial()
		if !preserveResume {
			s.clearSessionResumptionMemory()
		}
		cleanupErr = errors.Join(cleanupErr, s.closeWiresharkDebugger())
		if cleanupErr != nil {
			s.recordCleanupError(cleanupErr)
		}
		s.signalDone()
	})
}

func (s *Session) recordCleanupError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cleanupErr := fmt.Errorf("%w: %w", ErrSessionCleanup, err)
	s.cleanupErr = errors.Join(s.cleanupErr, cleanupErr)
	if s.terminalErr == nil {
		s.terminalErr = cleanupErr
		return
	}
	s.terminalErr = errors.Join(s.terminalErr, cleanupErr)
}

// ErrSessionCleanup marks a failure to release a local session resource
// (data plane, routes, TUN, kernel state).
var ErrSessionCleanup = errors.New("swu: clean up session")

// CleanupError returns the local cleanup failures recorded when the session
// was shut down, wrapping ErrSessionCleanup; nil when cleanup succeeded.
func (s *Session) CleanupError() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cleanupErr
}

// WaitDone blocks until the session is shut down.
func (s *Session) WaitDone() {
	<-s.done
}

// WaitDoneContext blocks until the session is shut down or the context ends.
func (s *Session) WaitDoneContext(ctx context.Context) error {
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Reauthenticate forces a full re-authentication (new IKE_SA_INIT).
func (s *Session) Reauthenticate() error {
	s.mu.RLock()
	established := s.state == stateEstablished
	s.mu.RUnlock()
	if !established {
		return errors.New("swu: session not established")
	}
	return ErrFreshRuntimeRequired
}

// InnerNetworkConfig is the address configuration assigned by the ePDG.
type InnerNetworkConfig struct {
	IPv4          net.IP
	IPv6          net.IP
	PrefixLen     int
	IPv6PrefixLen int
	DNS           []net.IP
	PCSCF         []net.IP
}

// InnerNetwork returns a copy of the negotiated inner network configuration.
func (s *Session) InnerNetwork() InnerNetworkConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return InnerNetworkConfig{
		IPv4: append(net.IP(nil), s.innerIP...), IPv6: append(net.IP(nil), s.innerIPv6...),
		PrefixLen: s.innerPrefix, IPv6PrefixLen: s.innerIPv6Prefix,
		DNS: cloneIPs(s.dnsServers), PCSCF: cloneIPs(s.pcscfServers),
	}
}

func (s *Session) primaryInnerIP() net.IP {
	if s == nil {
		return nil
	}
	ip, _ := InnerNetworkConfig{
		IPv4: s.innerIP, IPv6: s.innerIPv6,
		PrefixLen: s.innerPrefix, IPv6PrefixLen: s.innerIPv6Prefix,
		PCSCF: s.pcscfServers,
	}.PreferredIMSAddress()
	return ip
}

// IPv6IMSSkipReason is empty when the IPv6 inner address can be used for IMS.
// A non-empty value is why REGISTER must not bind that IPv6 (detect before Contact).
func (inner InnerNetworkConfig) IPv6IMSSkipReason() string {
	if inner.IPv6 == nil || inner.IPv6.To4() != nil {
		return "no_ipv6"
	}
	if inner.IPv6.IsUnspecified() || inner.IPv6.IsLinkLocalUnicast() || inner.IPv6.IsMulticast() {
		return "ipv6_not_unicast"
	}
	if inner.hasPCSCFFamily(false) {
		return ""
	}
	if inner.hasPCSCFFamily(true) {
		return "no_ipv6_pcscf"
	}
	return ""
}

func (inner InnerNetworkConfig) ipv4IMSSkipReason() string {
	if inner.IPv4.To4() == nil {
		return "no_ipv4"
	}
	if inner.hasPCSCFFamily(true) {
		return ""
	}
	if inner.hasPCSCFFamily(false) {
		return "no_ipv4_pcscf"
	}
	return ""
}

func (inner InnerNetworkConfig) hasPCSCFFamily(ipv4 bool) bool {
	for _, server := range inner.PCSCF {
		if server == nil {
			continue
		}
		if (server.To4() != nil) == ipv4 {
			return true
		}
	}
	return false
}

// PreferredIMSAddress is the IMS bind/Contact address. Dual-stack still
// prefers IPv6 when it is usable; IPv4 is only used when IPv6 cannot
// register (no matching P-CSCF, or not a unicast address).
func (inner InnerNetworkConfig) PreferredIMSAddress() (net.IP, int) {
	if inner.IPv6IMSSkipReason() == "" && inner.IPv6 != nil && inner.IPv6.To4() == nil {
		return inner.IPv6, inner.IPv6PrefixLen
	}
	if inner.ipv4IMSSkipReason() == "" && inner.IPv4.To4() != nil {
		return inner.IPv4, inner.PrefixLen
	}
	if inner.IPv4.To4() != nil {
		return inner.IPv4, inner.PrefixLen
	}
	return inner.IPv6, inner.IPv6PrefixLen
}

// InnerPacketIO returns the packet boundary for the user-space IMS stack.
func (s *Session) InnerPacketIO() InnerPacketIO {
	endpoint := s.currentInnerPacketEndpoint()
	if endpoint == nil {
		return nil
	}
	return endpoint
}

func cloneIPs(in []net.IP) []net.IP {
	out := make([]net.IP, 0, len(in))
	for _, ip := range in {
		out = append(out, append(net.IP(nil), ip...))
	}
	return out
}

// NextSequenceNumber returns the next ESP sequence number for the outbound SA.
func (s *Session) NextSequenceNumber() uint32 {
	s.childSAMu.RLock()
	defer s.childSAMu.RUnlock()
	if s.espOutboundSA == nil {
		return 0
	}
	return s.espOutboundSA.NextSequenceNumber()
}

// startTimers arms the rekey / reauth / keepalive / DPD timers.
func (s *Session) startTimers() {
	s.startIKEReauthTimer()
	ikeInterval, childInterval := s.rekeyIntervals()
	s.startIKESARekeyTimer(ikeInterval)
	s.startChildSARekeyTimer(childInterval)
	s.startNATKeepalive()
	s.startDPD()
	s.startXFRMExpireMonitor()
}

// stopTimers stops all timers.
func (s *Session) stopTimers() {
	s.timersMu.Lock()
	defer s.timersMu.Unlock()
	timers := []*time.Timer{s.ikeReauthTimer, s.ikeRekeyTimer, s.childRekeyTimer, s.natKeepalive, s.dpdTimer}
	for _, t := range timers {
		if t != nil {
			t.Stop()
		}
	}
	s.ikeReauthTimer = nil
	s.ikeRekeyTimer = nil
	s.childRekeyTimer = nil
	s.natKeepalive = nil
	s.dpdTimer = nil
	s.natKeepaliveStarted = false
}

func (s *Session) armTimer(target **time.Timer, delay time.Duration, callback func()) {
	s.timersMu.Lock()
	defer s.timersMu.Unlock()
	if s.ctx.Err() != nil {
		return
	}
	if *target != nil {
		(*target).Stop()
	}
	*target = time.AfterFunc(delay, callback)
}

// startIKEReauthTimer arms the periodic re-authentication timer.
func (s *Session) startIKEReauthTimer(intervals ...time.Duration) {
	every := configuredTimerInterval(intervals, s.cfg.ReauthSeconds, s.cfg.ReauthInterval)
	if s.authLifetime > 60 {
		dynamic := time.Duration(s.authLifetime-30) * time.Second
		if every <= 0 || dynamic < every {
			every = dynamic
		}
	}
	if every <= 0 {
		return
	}
	delay := every
	if jitterLimit := every / 10; jitterLimit > 0 {
		delay += time.Duration(rand.Int63n(int64(jitterLimit)))
	}
	s.armTimer(&s.ikeReauthTimer, delay, func() {
		s.triggerReauthentication()
	})
}

// startNATKeepalive arms the NAT keepalive timer (RFC 3948 §2.4).
func (s *Session) startNATKeepalive(intervals ...time.Duration) {
	if !s.natDetected {
		return
	}
	every := configuredTimerInterval(intervals, s.cfg.NATKeepaliveEvery, s.cfg.NATKeepaliveSeconds)
	if len(intervals) == 0 && every <= 0 {
		every = 20 * time.Second
	}
	if every <= 0 || !s.beginNATKeepalive() {
		return
	}
	s.initializeOutboundActivity()
	s.scheduleNATKeepalive(every, every)
}

// sendNATKeepalive sends a NAT keepalive packet on the ESP transport.
func (s *Session) sendNATKeepalive() error {
	transport := s.transport()
	if transport == nil {
		return errors.New("swu: no IKE transport")
	}
	if err := transport.SendNATKeepalive(); err != nil {
		return err
	}
	now := time.Now()
	s.mu.Lock()
	s.lastPingAt = now
	s.mu.Unlock()
	s.markOutboundActivityAt(now)
	return nil
}

// startDPD arms the dead-peer-detection timer (RFC 7296 §1.4.2).
func (s *Session) startDPD() {
	every := configuredDuration(s.cfg.DPDProbeEvery, s.cfg.DPDIntervalSeconds)
	if every <= 0 {
		return
	}
	s.startDPDWithInterval(every)
}

func (s *Session) startDPDWithInterval(every time.Duration) {
	if every <= 0 {
		return
	}
	s.initializeInboundActivity()
	s.scheduleDPD(every, every)
}

func (s *Session) scheduleDPD(every, delay time.Duration) {
	s.armTimer(&s.dpdTimer, delay, func() {
		if idle := s.inboundIdleDuration(); idle < every {
			s.scheduleDPD(every, every-idle)
			return
		}
		if err := s.DPDProbe(); err != nil {
			s.failEstablishedControl(fmt.Errorf("swu: DPD failed: %w", err))
			return
		}
		s.scheduleDPD(every, every)
	})
}

// DPDProbe sends an INFORMATIONAL request to verify the peer is alive.
func (s *Session) DPDProbe() error {
	s.ikeExchangeMu.Lock()
	defer s.ikeExchangeMu.Unlock()
	if s.transport() == nil {
		return errors.New("swu: no transport")
	}
	s.mu.Lock()
	s.lastDPDAt = time.Now()
	s.mu.Unlock()
	response, err := s.sendEncryptedWithRetry(nil, ikev2.INFORMATIONAL)
	if err != nil {
		return err
	}
	packet, err := ikev2.DecodePacket(response)
	if err != nil {
		return fmt.Errorf("swu: decode DPD response: %w", err)
	}
	payloads, err := s.decryptAndParse(packet)
	if err != nil {
		return err
	}
	if len(payloads) != 0 {
		return fmt.Errorf("swu: DPD response contains unexpected payloads %s", ikePayloadTypes(payloads))
	}
	return nil
}

// nextMessageID returns the next initiator request ID. IKE_SA_INIT uses zero;
// subsequent exchanges start at one and increase monotonically (RFC 7296).
func (s *Session) nextMessageID() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextOutboundID
	s.nextOutboundID++
	s.SequenceNumber.Store(s.nextOutboundID)
	return id
}

// StartDPD starts the dead-peer-detection timer (RFC 7296 §1.4.2).
func (s *Session) StartDPD(intervals ...time.Duration) {
	if s == nil {
		return
	}
	if len(intervals) > 0 && intervals[0] > 0 {
		s.startDPDWithInterval(intervals[0])
		return
	}
	s.startDPD()
}
