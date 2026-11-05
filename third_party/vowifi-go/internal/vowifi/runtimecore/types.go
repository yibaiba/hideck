// Package runtimecore prepares and owns a complete SWu and IMS runtime session.
package runtimecore

import (
	"context"
	"errors"

	"github.com/iniwex5/vowifi-go/engine/swu"
	"github.com/iniwex5/vowifi-go/internal/vowifi/access"
	"github.com/iniwex5/vowifi-go/internal/vowifi/epdg"
	"github.com/iniwex5/vowifi-go/internal/vowifi/events"
	"github.com/iniwex5/vowifi-go/internal/vowifi/imscore"
	"github.com/iniwex5/vowifi-go/internal/vowifi/imsendpoint"
	"github.com/iniwex5/vowifi-go/internal/vowifi/netstack"
	"github.com/iniwex5/vowifi-go/internal/vowifi/profile"
	"github.com/iniwex5/vowifi-go/internal/vowifi/smsdelivery"
)

type ErrRedirect struct {
	NewEPDG string
	Delay   int64
	// Target preserves the interim field name for callers compiled after v1.5.5.
	Target string
}

func (err ErrRedirect) Error() string {
	return "redirect to " + firstNonEmpty(err.NewEPDG, err.Target)
}

const maxSWuRedirects = 3

var ErrTooManyRedirects = errors.New("runtimecore: too many ePDG redirects")

type InterruptOutcome struct {
	Kind         string
	Reason       string
	RedirectEPDG string
	RetryDelay   int64
}

type ProxyConfig struct {
	ID       string
	Addr     string
	Username string
	Password string
	Enabled  bool
}

type RuntimeDataplanePolicy struct {
	Mode    string
	TUNName string
}

type RuntimeEvent[T any] struct {
	Kind         string
	Handle       T
	DeviceID     string
	TraceID      string
	Identity     profile.IMSIdentityResult
	Snapshot     Snapshot
	Service      *imscore.Service
	Message      string
	Reason       string
	Attempt      int
	RetryDelay   int64
	RedirectEPDG string
}

type RuntimeEventSink[T any] interface {
	OnRuntimeEvent(context.Context, RuntimeEvent[T])
}

type RuntimeObserver interface {
	OnRuntimeEvent(context.Context, RuntimeEvent[*SessionResult])
}

type RuntimeHostHooks struct {
	Events                RuntimeEventSink[*SessionResult]
	OnPrepared            func(context.Context, profile.PreparedSession)
	OnConnecting          func(context.Context)
	OnEstablished         func(context.Context, RuntimeStartResult)
	OnInterruptReady      func(context.Context)
	OnSessionDown         func(context.Context)
	OnReauthNeeded        func(context.Context)
	OnRedirect            func(context.Context, string)
	OnInterrupted         func(context.Context, error)
	OnRetryDelay          func(context.Context, int, int64)
	OnIMSRegistered       func(context.Context)
	OnSMSReady            func(context.Context)
	OnSMSReadinessChanged func(context.Context, imscore.SMSReadiness)
	OnError               func(context.Context, error)
	OnStopped             func(context.Context)
}

type RuntimeOptions struct {
	Voice VoiceLifecycle
}

type RuntimeStartResult struct {
	TraceID string
	Session *SessionResult
}

type SessionConfig struct {
	Ctx                   context.Context
	DeviceID              string
	TraceID               string
	Prepared              profile.PreparedSession
	SIM                   access.SIMAdapter
	DataplaneMode         string
	TUNName               string
	Proxy                 *ProxyConfig
	DNSServer             string
	DeliveryStore         smsdelivery.Store
	Dispatch              events.EventDispatcher
	OnIMSRegistered       func()
	OnSMSReady            func()
	OnSMSReadinessChanged func(imscore.SMSReadiness)
	ResumeTicket          []byte
	ResumeOldSKd          []byte
	OnTicketUpdate        func([]byte, []byte)
	FastReauthID          string
	FastReauthMK          []byte
	FastReauthKAut        []byte
	FastReauthKEncr       []byte
	OnFastReauthUpdate    func(string, []byte, []byte, []byte)
	OnProgress            func(string)
	OnTunnelReady         func(*SessionResult)
	OmitInitialContact    bool
	RegistrarPenalties    *imscore.RegistrarPenaltyStore
	EPDGCandidates        *swu.EPDGCandidateStore

	SubscriptionRegistrations *imscore.SubscriptionRegistrationStore
	MWIPolicyStore            imscore.MWISubscriptionPolicyStore
}

type SessionResult struct {
	notifications *candidateNotifications
	DeviceID      string
	EPDGMgr       *epdg.Manager
	Session       *swu.Session
	Snapshot      swu.SessionSnapshot
	IMSNetwork    *netstack.Network
	IMSService    *imscore.Service
	LocalAddr     string
	XCAPRequired  bool
	XCAPSession   *swu.Session
	XCAPNetwork   *netstack.Network
	Proxy         *ProxyConfig
	interrupts    chan InterruptOutcome
}

type Snapshot struct {
	Established bool
	InnerIP     string
	TUNName     string
}

type RuntimeStartRequest struct {
	DeviceID            string
	TraceID             string
	Profile             profile.Profile
	Prepared            *profile.PreparedSession
	RuntimeEPDGOverride string
	IMSIdentity         profile.IMSIdentityResult
	SIM                 access.SIMAdapter
	Access              access.Adapter
	Dataplane           RuntimeDataplanePolicy
	Proxy               *ProxyConfig
	DNSServer           string
	DeliveryStore       smsdelivery.Store
	SubscriptionStore   imscore.SubscriptionRejectionPersistence
	MWIPolicyStore      imscore.MWISubscriptionPolicyStore
	Dispatch            events.EventDispatcher
	Reconnect           bool
	ReconnectDelay      func(int) int64
	Observer            RuntimeObserver
	Hooks               RuntimeHostHooks
	Options             RuntimeOptions
	OnProgress          func(string)
	BeforeSessionStart  func(context.Context, SessionConfig) error
	SessionStarter      func(context.Context, SessionConfig) (*SessionResult, error)
	StopSession         func(context.Context, *SessionResult)
	ShouldRun           func() bool
	DryRun              bool
	voiceBinding        *voiceLifecycleBinding
	redirectHops        int
	redirectSeen        []string
	fastReauth          *FastReauthStore
	omitInitialContact  bool
	registrarPenalties  *imscore.RegistrarPenaltyStore
	epdgCandidates      *swu.EPDGCandidateStore

	subscriptionRegistrations *imscore.SubscriptionRegistrationStore
}

type VoiceLifecycle interface {
	AttachDevice(string, imsendpoint.Endpoint) error
	DetachDevice(string)
}

type Runtime struct{}
