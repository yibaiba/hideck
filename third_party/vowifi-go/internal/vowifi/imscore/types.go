// Package imscore is the IMS core: SIP registration (Digest-AKA), dialog
// management, and SMS/USSD-over-IMS.
//
// Reconstructed from the decompiled internal/vowifi/imscore (RFC 3261, RFC
// 2617, RFC 3310, 3GPP TS 24.229, TS 24.390).
package imscore

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	enginesim "github.com/iniwex5/vowifi-go/engine/sim"
	"github.com/iniwex5/vowifi-go/internal/vowifi/common"
	"github.com/iniwex5/vowifi-go/internal/vowifi/smsdelivery"
	"github.com/iniwex5/vowifi-go/internal/vowifi/ussi"
)

// IMS registration states (recovered from the decompiled registration_state.go).
const (
	regIdle        = "idle"
	regRegistering = "registering"
	regRegistered  = "registered"
	regReregister  = "reregistering"
	regFailed      = "failed"
	regUnregister  = "unregistering"
)

// IMSRegisterTemplate is the carrier-specific REGISTER wire policy.
type IMSRegisterTemplate struct {
	Expires                   time.Duration
	Transport                 string
	SupportedHeader           string
	AllowHeader               string
	ContactMode               string
	AccessType                string
	ICSIRef                   string
	ContactOrder              []string
	IncludePANIAuthenticated  bool
	StrictSecurityServerOffer bool
}

// AKAProvider computes AKA from the network challenge.
type AKAProvider = enginesim.AKAProvider

// AKAResult is the outcome of an AKA computation.
type AKAResult = enginesim.AKAResult

// DialOptions controls a connection created on an IMS network.
//
// The fields mirror the original network boundary. Timeout and KeepAlive are
// durations represented as nanoseconds; TCPMSS overrides the endpoint MSS when
// it is positive.
type DialOptions struct {
	Timeout   int64
	KeepAlive int64
	TCPMSS    int
}

// IMSNetwork is the network surface used by the IMS stack.
type IMSNetwork interface {
	LocalIP() net.IP
	HasLocalIP(ip net.IP) bool
	ResolveIP(ctx context.Context, host string) (net.IP, error)
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
	DialTCPContext(ctx context.Context, local, remote *net.TCPAddr) (net.Conn, error)
	ListenTCP(addr *net.TCPAddr) (net.Listener, error)
	ListenPacket(network string, addr *net.UDPAddr) (net.PacketConn, error)
}

// SystemIMSNetwork is the default IMS network implementation.
type SystemIMSNetwork struct {
	localIP net.IP
}

// NewSystemIMSNetwork creates a network with the given local IP.
func NewSystemIMSNetwork(localIP net.IP) *SystemIMSNetwork {
	return &SystemIMSNetwork{localIP: localIP}
}

// LocalIP returns the local IP.
func (n *SystemIMSNetwork) LocalIP() net.IP { return n.localIP }

// HasLocalIP reports whether the network has the address.
func (n *SystemIMSNetwork) HasLocalIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if n.localIP != nil && n.localIP.Equal(ip) {
		return true
	}
	return common.HostHasIP(ip.String())
}

// ResolveIP resolves a host to an IP.
func (n *SystemIMSNetwork) ResolveIP(ctx context.Context, host string) (net.IP, error) {
	ips, err := n.LookupIPs(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) > 0 {
		return ips[0], nil
	}
	return nil, net.ErrClosed
}

// LookupIPs returns every resolved address so security policy can retain the
// local address family when DNS returns mixed A and AAAA records.
func (n *SystemIMSNetwork) LookupIPs(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// LookupSRV resolves a SIP service endpoint.
func (n *SystemIMSNetwork) LookupSRV(ctx context.Context, service, proto, name string) (string, uint16, error) {
	_, records, err := net.DefaultResolver.LookupSRV(ctx, service, proto, name)
	if err != nil {
		return "", 0, err
	}
	if len(records) == 0 {
		return "", 0, errors.New("imscore: no SRV records")
	}
	return strings.TrimSuffix(records[0].Target, "."), records[0].Port, nil
}

// DialContext dials a TCP connection.
func (n *SystemIMSNetwork) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// DialTCPContext dials TCP from an explicit local IMS address and port.
func (n *SystemIMSNetwork) DialTCPContext(ctx context.Context, local, remote *net.TCPAddr) (net.Conn, error) {
	dialer := net.Dialer{LocalAddr: local}
	return dialer.DialContext(ctx, "tcp", remote.String())
}

// ListenTCP listens for TCP connections.
func (n *SystemIMSNetwork) ListenTCP(addr *net.TCPAddr) (net.Listener, error) {
	return net.ListenTCP("tcp", addr)
}

// ListenPacket listens for UDP packets.
func (n *SystemIMSNetwork) ListenPacket(network string, addr *net.UDPAddr) (net.PacketConn, error) {
	return net.ListenUDP("udp", addr)
}

// Service is the IMS core service.
type Service struct {
	cfg *IMSConfig
	registrationRuntime
	messagingRuntime
	observability
	pingState

	mu          sync.RWMutex
	registerMu  sync.Mutex
	subscribeMu sync.Mutex
	state       string
	regState    string

	// Registration state.
	regSession *registerSession
	spiPairs   [][2]uint32

	// Set when this REGISTER flow moved an oversized UDP request to TCP, and
	// when that TCP failed so the flow retries over UDP without moving again.
	largeRegisterMovedToTCP atomic.Bool
	largeRegisterTCPFailed  atomic.Bool

	// SIP transport.
	transport                 *sipTransport
	registrationIO            net.PacketConn
	registrationTCP           net.Conn
	registrationPreviousTCP   net.Conn
	registrationTCPProtected  bool
	registrationTransport     string
	securityServerIO          net.Listener
	protectedUDP              *protectedUDPTransport
	udpDownlinkProven         atomic.Bool
	rpReportRegisteredFlow    atomic.Bool // RP reports reach the SMSC only over the registered flow
	clientPortReserve         net.Listener
	registrationRemote        *net.UDPAddr
	protectedClientPort       int
	protectedServerPort       int
	externalTransport         bool
	protectedConnMu           sync.Mutex
	protectedConns            map[net.Conn]struct{}
	portSSessionMu            sync.Mutex
	portSSession              portSSessionState
	portSReconnectGrace       time.Duration
	portSFailoverVerifyWait   time.Duration
	portSLastReadAt           atomic.Int64
	portSPushReady            atomic.Bool
	portSReconnectWaiting     atomic.Bool
	portSRecoveryPending      atomic.Bool
	portSRecoveryAwaitingFlow atomic.Bool
	portSOnDemandObserved     atomic.Bool
	portSWatchMu              sync.Mutex
	portSWatchTimer           *time.Timer
	portSWatchGeneration      uint64
	portSBackoff              portSRecoveryBackoff
	portSRecoveryJitter       func(time.Duration) time.Duration
	registrationGeneration    atomic.Uint64
	sipWriteMu                sync.Mutex
	receiverMu                sync.Mutex
	activeReceivers           int
	inboundStatsMu            sync.Mutex
	inboundStatsCancel        context.CancelFunc
	inboundStatsDone          chan struct{}
	downlinkValidationWake    chan struct{}
	downlinkGeneration        uint64
	downlinkRequests          uint64
	smsReception              smsReceptionEvidence
	registerDownlinkBaseline  downlinkCheckpoint
	replacementDownlinkWatch  *replacementDownlinkWatch
	networkDone               sync.WaitGroup
	registerErrors            chan error
	keepaliveOnce             sync.Once
	keepaliveSuccessOnce      sync.Once
	maintenanceWake           chan struct{}
	registrationRefreshAt     time.Time
	subscriptionRefreshAt     time.Time
	subscriptionLastAttemptAt time.Time
	subscriptionLastOKAt      time.Time
	subscriptionExpires       time.Duration
	subscriptionLastErr       string
	subscriptionInFlight      atomic.Bool
	subscriptionClosed        bool
	subscriptionDialog        registrationSubscriptionDialog
	subscriptionLifecycle     subscriptionLifecycle
	mwiSubscriptionLifecycle  subscriptionLifecycle
	subscriptionGeneration    uint64
	subscriptionBinding       subscriptionRegistration
	subscriptionRegistrations *SubscriptionRegistrationStore
	notifyReconnectPending    atomic.Bool
	bindingCleanupPending     atomic.Bool
	lastRegisterContactCount  atomic.Int32
	keepaliveInterval         time.Duration
	keepaliveTimeout          time.Duration
	keepaliveFailureLimit     int
	tcpKeepalivePong          chan error
	tcpCRLFPongWait           time.Duration
	sipOutboundKeepalive      bool
	sipOutbound               bool
	sipOutboundRequired       bool
	outboundContactOffered    bool
	outboundContactRegistered bool
	flowTimer                 time.Duration
	stunKeepaliveWait         chan stunKeepaliveResult
	stunKeepaliveTxID         [12]byte
	stunMappedAddr            *net.UDPAddr
	peerAllow                 []string
	peerICSI                  []string
	peerCapabilityAfter       time.Time
	peerCapabilityDone        bool
	stunRTO                   time.Duration
	stunRc                    int
	stunKeepaliveInterval     time.Duration

	// Dialogs.
	dialogRegistry *dialogRegistry
	endpointCSeq   atomic.Uint32
	serverTxMu     sync.Mutex
	serverTx       map[string]trackedServerTransaction
	serverTimers   serverTransactionTimers

	// Event buses. The runtime bus carries host notifications; the IMS bus
	// carries the original endpoint-level SIP events and is service-owned.
	bus             *EventBus
	imsEventBusOnce sync.Once
	imsEventBus     *imsEventBus

	// USSD.
	ussd *ussi.Service

	// Voice request routing.
	voiceHandler VoiceRequestHandler

	// Delivery store.
	delivery  DeliveryStore
	smsRandom io.Reader

	// Callbacks and SMS capability state.
	onRegistered           func()
	onSMSReady             func()
	onSMSReadiness         func(SMSReadiness)
	smsReceiverReady       bool
	smsReadinessReason     atomic.Value
	smsMemoryFull          bool
	smsMemoryDenied        bool
	smsMemoryDeniedGateway string
	smsReadyNotified       bool
	nextSIPCSeq            int
	smsTransactionTimeout  time.Duration
	smsReportTimeout       time.Duration
	fragmentState

	securityVerify         string
	effectiveSecurityMode  string
	securityFallbackReason string
	securityFallbackCount  atomic.Int64
	signalingGeneration    uint64
	signalingReady         bool
	signalingFailureReason string
	lastError              string
	serviceRoute           string
	path                   string
	pubGRUU                string
	tempGRUU               string
	assocMSISDN            string
	learnedAOR             string
	reginfoAOR             string

	stop chan struct{}

	// RFC 3842 MWI subscription. Independent of the RFC 3680 reg dialog.
	mwiSubscriptionDialog        registrationSubscriptionDialog
	mwiSubscriptionRefreshAt     time.Time
	mwiSubscriptionLastAttemptAt time.Time
	mwiSubscriptionLastOKAt      time.Time
	mwiSubscriptionExpires       time.Duration
	mwiSubscriptionLastErr       string
	mwiSubscriptionInFlight      atomic.Bool
	mwiSubscriptionClosed        bool
	mwiLastSummary               string
	mwiMessagesWaiting           bool
}

type registrationRuntime struct {
	callID               string
	fromTag              string
	cseq                 atomic.Uint32
	expires              uint32
	authRealm            string
	challengeRealm       string
	registrar            string
	registrarCandidates  []string
	registrarIndex       int
	registrarSource      string
	registrarPenalties   *RegistrarPenaltyStore
	regStatus            atomic.Int32
	nextRegister         time.Time
	lastSIPCode          atomic.Int32
	lastSIPText          string
	reRegisterPending    atomic.Bool
	regFailCount         atomic.Int32
	OnReconnectNeeded    func()
	reconnectTriggering  atomic.Bool
	pcscfRecoveryPending atomic.Bool

	registrarRecoveryAttempt registrarRecoveryAttempt
}

type pingState struct {
	pingFailCount atomic.Int32
	lastPingAt    time.Time
	lastPingOK    atomic.Bool
	pingSending   atomic.Bool
}

// SMSReadiness describes the independently verifiable IMS SMS prerequisites.
type SMSReadiness struct {
	ReceiverPending bool
	Registered      bool
	ProfileReady    bool
	TransportReady  bool
	ReceiverReady   bool
	SMSCPresent     bool
	Ready           bool
	MOReady         bool
	HealthReady     bool
	Reason          string
}

// ServiceStatus is a snapshot of the IMS service state.
type ServiceStatus struct {
	Enabled                bool
	DeviceID               string
	Registered             bool
	RegStatus              string
	Registrar              string
	RegistrarCandidates    []string
	RegistrarIndex         int
	RegistrarSource        string
	RegistrationGeneration uint64
	RegistrationRegID      int
	LastSIPCode            int
	LastSIPText            string
	Domain                 string
	IMPI                   string
	IMPU                   string
	Transport              string
	SMSReceiverTransport   string
	LocalAddr              string
	LocalPort              int
	PortSConnected         bool
	PortSGeneration        uint64
	PortSOpenedAt          time.Time
	PortSClosedAt          time.Time
	PortSLastInboundAt     time.Time
	PortSLastCloseKind     string
	PortSLastCloseReason   string
	PortSPeerResetCount    uint64
	DeprioritizedPCSCF     map[string]time.Time
	IPSecInstalled         bool
	RXRunning              bool
	RXPort                 int
	TCPSignalingRunning    bool
	TCPSignalingConnected  bool
	EffectiveSecurityMode  string
	SecurityFallbackReason string
	SecurityFallbackCount  int64
	SignalingGeneration    uint64
	SignalingReady         bool
	SignalingFailureReason string
	RegFailCount           int
	ReRegisterPending      bool
	PingFailCount          int
	LastPingAt             time.Time
	LastPingOK             bool
	LastRegisterTraceID    string
	LastRegisterAttemptAt  time.Time
	LastRegisterOKAt       time.Time
	LastRegisterErr        string
	LastSMSSendTraceID     string
	LastSMSSendAt          time.Time
	LastSMSSendErr         string
	ServiceRoute           string
	Path                   string
	SecurityVerify         string
	AssociatedMSISDN       string
	LastError              string
	FragmentAudit          map[string]interface{}
	IMSEventBus            map[string]interface{}
	Diagnostics            map[string]interface{}

	// Compatibility fields added after v1.5.5.
	State    string
	RegState string
	IMPUs    []string
}

// IsRegistered reports whether the service is registered.
func (s ServiceStatus) IsRegistered() bool {
	return s.Registered || strings.EqualFold(strings.TrimSpace(s.RegStatus), "Registered")
}

// DeliveryStore persists SMS delivery state.
type DeliveryStore = smsdelivery.Store

// SMSDeliverySIPResultStore persists the final response to the outbound
// MESSAGE transaction separately from the later RP delivery report.
type SMSDeliverySIPResultStore = smsdelivery.SIPResultStore

// SMSInboundFragmentStore persists incomplete inbound multipart SMS state.
type SMSInboundFragmentStore = smsdelivery.InboundFragmentStore

// SMSInboundFragmentLifecycleStore persists the one-shot degraded notification state.
type SMSInboundFragmentLifecycleStore = smsdelivery.InboundFragmentLifecycleStore

type smsInboundFragmentOwner = smsdelivery.InboundFragmentOwner
type smsInboundFragmentScope = smsdelivery.InboundFragmentScope
type smsInboundFragmentRecord = smsdelivery.InboundFragment
type storedSMSInboundFragment = smsdelivery.StoredInboundFragment
type smsInboundFragmentSaveResult = smsdelivery.InboundFragmentSaveResult

// DeliveryPartMatch identifies a delivery part.
type DeliveryPartMatch = smsdelivery.DeliveryPartMatch

// DeliveryStatus is the SMS delivery status.
type DeliveryStatus = smsdelivery.DeliveryStatus
type DeliveryRecord = smsdelivery.DeliveryRecord
type SMSDeliveryIdentityStore = smsdelivery.IdentityStore

// DeliveryPartStatus is one delivery part.
type DeliveryPartStatus = smsdelivery.DeliveryPartStatus
