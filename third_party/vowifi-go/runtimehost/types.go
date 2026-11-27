// Package runtimehost owns the public VoWiFi runtime lifecycle.
package runtimehost

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	enginesim "github.com/iniwex5/vowifi-go/engine/sim"
	"github.com/iniwex5/vowifi-go/engine/swu"
	"github.com/iniwex5/vowifi-go/internal/vowifi/access"
	"github.com/iniwex5/vowifi-go/internal/vowifi/runtimecore"
	"github.com/iniwex5/vowifi-go/runtimehost/identity"
	"github.com/iniwex5/vowifi-go/runtimehost/messaging"
)

const ErrorClassReauthentication = "reauth"

// State retains the exact v1.5.5 field prefix. Current projections follow it.
type State struct {
	Phase            string
	DeviceID         string
	DataplaneMode    string
	NetworkMode      string
	SIMReady         bool
	AccessReady      bool
	TunnelReady      bool
	IMSReady         bool
	SMSReady         bool
	RegStatus        int
	RegStatusText    string
	LastEvent        string
	LastReason       string
	LastRedirectEPDG string
	LastErrorClass   string
	LastError        string
	UpdatedAt        time.Time

	SessionState   string
	IMSState       string
	Error          string
	DataPlaneUp    bool
	NATDetected    bool
	EPDGAddress    string
	SMSReadyReason string
	SMSHealthReady bool
	SMSRXPending   bool
	SMSMOReady     bool
}

// Event retains the recovered runtime event payload before additive fields.
type Event struct {
	Kind         string
	DeviceID     string
	TraceID      string
	Reason       string
	Attempt      int
	RetryDelay   int64
	RedirectEPDG string
	State        State

	Type    string
	Detail  string
	Session *Instance
}

type Observer interface {
	OnRuntimeHostEvent(context.Context, Event)
}

type ObserverFunc func(context.Context, Event)
type Notifier func(string)
type SMSNotifier func(string, string, string, time.Time)

// Service is the original root service surface.
type Service = messaging.Service

type SendOptions = messaging.SendOptions
type SendOutcome = messaging.SendOutcome
type DeliveryStatus = messaging.DeliveryStatus
type USSDResult = messaging.USSDResult

// Status is retained for callers of the post-v1.5.5 lifecycle injection API.
type Status struct {
	State State
}

// CurrentService is the displaced lifecycle injection surface.
type CurrentService interface {
	SendSMSWithOptions(context.Context, string, string, messaging.SendOptions) (messaging.SendOutcome, error)
	SendSMSWithResult(context.Context, string, string) (messaging.SendOutcome, error)
	GetSMSDeliveryStatus(context.Context, string) (*messaging.DeliveryStatus, error)
	SendUSSD(context.Context, string) (*messaging.USSDResult, error)
	ContinueUSSD(context.Context, string, string) (*messaging.USSDResult, error)
	CancelUSSD(context.Context, string) error
	Status() Status
	StatusSnapshot() Status
	Stop()
	TriggerRegisterImmediate() error
}

// Modem is the exact modem boundary used by the original runtime host.
type Modem interface {
	CloseLogicalChannel(int) error
	DeviceID() string
	ExecuteATSilent(string, time.Duration) (string, error)
	GetNetworkMode() string
	GetRegStatus() (int, string)
	IsHealthy() bool
	IsSimInserted() bool
	OpenLogicalChannel(string) (int, error)
	QuerySIMInserted() (bool, error)
	Stop()
	TransmitAPDU(int, string) (string, error)
}

type ModemCapabilities = identity.AccessCapabilities
type IMSIdentityProvider = identity.IMSIdentityProvider

// SIMAdapter deliberately has an unexported method, matching v1.5.5. Hosts
// obtain one through NewReaderSIMAdapter instead of implementing it directly.
type SIMAdapter interface {
	runtimeSIMAdapter() access.SIMAdapter
}

type Tunnel interface {
	Connect(context.Context) error
	Shutdown()
	State() string
	WaitDoneContext(context.Context) error
	InnerNetwork() swu.InnerNetworkConfig
	InnerPacketIO() swu.InnerPacketIO
	UpdateAddresses(net.IP, net.IP) error
}

type tunnelFailureSource interface {
	TerminalError() error
}

// IMSLifecycle is additive and only used by explicitly injected current factories.
type IMSLifecycle interface {
	CurrentService
	Register(context.Context) error
}

type registrationFailureSource interface {
	RegistrationErrors() <-chan error
}

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

type smsReadinessSource interface {
	SMSReadiness() SMSReadiness
	SetOnSMSReadinessChanged(func(SMSReadiness))
}

type IMSFactory func(StartRequest, Tunnel) (IMSLifecycle, error)
type TunnelFactory func(*swu.Config) (Tunnel, error)

type ProxyConfig struct {
	ID       string
	Addr     string
	Username string
	Password string
	Enabled  bool
}

type SessionConfig struct {
	Ctx           context.Context
	DeviceID      string
	TraceID       string
	Prepared      identity.PreparedSession
	DataplaneMode string
	TUNName       string
	Proxy         *ProxyConfig
	DNSServer     string
}

type DataplanePolicy struct {
	Mode    string
	TUNName string
}

type StartResult struct {
	TraceID string
}

var ErrAPDUBusy error = apduBusyError{}

type apduBusyError struct{}

func (apduBusyError) Error() string { return "runtimehost: APDU channel busy" }
func (apduBusyError) Unwrap() error { return enginesim.ErrAPDUBusy }

var errNoService = errors.New("runtimehost: no service installed")
var errNoIdentityProvider = errors.New("runtimehost: no identity provider")

const (
	PhaseSIMReady    = "sim_ready"
	PhaseAccessReady = "access_ready"
)

// Instance retains the v1.5.5 field sequence before additive fields.
type Instance struct {
	mu        sync.RWMutex
	state     State
	service   messaging.Service
	session   *runtimecore.SessionResult
	startedAt time.Time
	cancel    func()
	observers []Observer
	onNotify  func(string)
	onSMS     func(string, string, string, time.Time)

	tunnel      Tunnel
	voiceDetach func() error
	stopped     bool
	// exited is closed when the runtime core runner goroutine returns; nil
	// for instances that have no background runner.
	exited <-chan struct{}
	// stopDone is closed when the release task started by Stop has finished;
	// stopErr is its result.
	stopDone         chan struct{}
	stopErr          error
	runnerCleanupErr error
}

func NewTraceID() string { return newTraceID() }

type accessAdapter struct {
	host identity.AccessAdapter
}

type identityProviderAdapter struct {
	provider identity.IMSIdentityProvider
}

type modemAccessAdapter struct {
	modem Modem
}
