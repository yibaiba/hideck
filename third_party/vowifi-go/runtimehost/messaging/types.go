// Package messaging defines the SMS delivery and USSD result types shared
// between the IMS host and the vohive delivery store.
//
// Reconstructed from the decompiled engine/runtimehost/messaging.
package messaging

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/internal/vowifi/smsdelivery"
)

// ErrDeliveryNotFound is returned when a delivery record does not exist.
var ErrDeliveryNotFound = errors.New("sms delivery not found")

// ErrSMSNotReady allows hosts to wait across a transient IMS runtime recovery.
var ErrSMSNotReady = smsdelivery.ErrSMSNotReady

// DeliveryStatus is the delivery status of an SMS.
type DeliveryStatus struct {
	MessageID  string               `json:"message_id"`
	IMSI       string               `json:"imsi"`
	DeviceID   string               `json:"device_id"`
	Peer       string               `json:"peer"`
	Content    string               `json:"content"`
	PartsTotal int                  `json:"parts_total"`
	Acks       int                  `json:"acks"`
	State      string               `json:"state"`
	LastError  string               `json:"last_error"`
	CreatedAt  time.Time            `json:"created_at"`
	UpdatedAt  time.Time            `json:"updated_at"`
	Parts      []DeliveryPartStatus `json:"parts"`
	ICCID      string               `json:"iccid"`
}

// DeliveryPartStatus is the status of one SMS part.
type DeliveryPartStatus struct {
	PartNo      int        `json:"part_no"`
	CallID      string     `json:"call_id"`
	InReplyTo   string     `json:"in_reply_to"`
	RPMR        int        `json:"rp_mr"`
	State       string     `json:"state"`
	SIPCode     int        `json:"sip_code"`
	RPCause     int        `json:"rp_cause"`
	RPCauseText string     `json:"rp_cause_text,omitempty"`
	ErrorText   string     `json:"error_text"`
	SentAt      time.Time  `json:"sent_at"`
	ReportAt    *time.Time `json:"report_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// DeliveryPartMatch is the result of matching a delivery report to a part.
type DeliveryPartMatch struct {
	MessageID string
	PartNo    int
	State     string
	Matched   bool
}

// SendOptions carries optional SMS delivery parameters.
type SendOptions struct {
	Encoding string `json:"encoding,omitempty"`

	// SuppressSendTGSuccess is an additive host notification option.
	SuppressSendTGSuccess bool `json:"-"`
}

// SendOutcome is the result of an SMS send.
type SendOutcome struct {
	MessageID     string `json:"message_id"`
	PartsTotal    int    `json:"parts_total"`
	DeliveryState string `json:"delivery_state"`

	// Ref and Err preserve the current Go API without changing the recovered wire shape.
	Ref string `json:"-"`
	Err error  `json:"-"`

	SIPCode             int  `json:"sip_code,omitempty"`
	RecommendCSFallback bool `json:"recommend_cs_fallback,omitempty"`
}

// USSDResult is the result of a USSD operation.
type USSDResult struct {
	Status    int    `json:"status"`
	Text      string `json:"text"`
	RawXML    string `json:"raw_xml,omitempty"`
	DCS       int    `json:"dcs"`
	SessionID string `json:"session_id,omitempty"`

	// These fields preserve the current result projection.
	RawText string `json:"raw_text,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// WithSuppressSendTGSuccess returns a context carrying the suppress-success
// option for SMS sends.
func WithSuppressSendTGSuccess(ctx context.Context) context.Context {
	return context.WithValue(ctx, suppressSendTGSuccessKey{}, true)
}

type suppressSendTGSuccessKey struct{}

// SuppressSendTGSuccess reports whether the context suppresses the success
// notification.
func SuppressSendTGSuccess(ctx context.Context) bool {
	v, _ := ctx.Value(suppressSendTGSuccessKey{}).(bool)
	return v
}

// RPCauseText maps an RP cause code to its 3GPP TS 24.011 text.
func RPCauseText(rpCause int) string {
	switch rpCause {
	case 0:
		return "normal"
	case 1:
		return "unassigned number"
	case 8:
		return "operator determined barring"
	case 10:
		return "call barred"
	case 21:
		return "short message transfer rejected"
	case 29:
		return "facility rejected"
	case 38:
		return "network out of order"
	case 41:
		return "temporary failure"
	case 69:
		return "requested facility not implemented"
	case 95:
		return "semantically incorrect message"
	case 111:
		return "protocol error"
	default:
		return "unknown"
	}
}

// DeliveryStore persists SMS delivery state.
type DeliveryStore interface {
	CreateSMSDelivery(messageID, imsi, deviceID, peer, content string, partsTotal int, at time.Time) error
	UpsertSMSDeliveryPart(messageID string, partNo int, callID string, rpMR int, state string, sentAt time.Time) error
	MarkSMSDeliveryPartReport(inReplyTo, callID, deviceID string, rpMR int, state string, sipCode int, rpCause int, errText string, at time.Time) (DeliveryPartMatch, error)
	RecomputeSMSDelivery(messageID string, at time.Time) error
	UpdateSMSDeliveryState(messageID, state, lastError string, acks int, at time.Time) error
	GetSMSDeliveryStatus(messageID string) (*DeliveryStatus, error)
}

type IdentityDeliveryStore interface {
	CreateSMSDeliveryForIdentity(record DeliveryRecord) error
}

// SubscriptionRejectionStore persists explicit 489 Bad Event responses across
// runtime and process restarts. Expiration follows the associated IMS
// registration lifetime so a genuinely new registration may probe again.
type SubscriptionRejectionStore interface {
	LoadIMSSubscriptionRejection(identity, eventPackage string, now time.Time) (status int, expiresAt time.Time, err error)
	SaveIMSSubscriptionRejection(identity, eventPackage string, status int, expiresAt time.Time) error
	DeleteIMSSubscriptionRejections(identity string) error
}

// MWISubscriptionPolicyStore is an optional delivery-store capability that
// supplies the per-SIM MWI setting (auto, on, off) and remembers identities
// whose network explicitly rejected SUBSCRIBE(mwi) with 405 or 489. A
// remembered rejection has no expiry; turning the setting on clears it.
type MWISubscriptionPolicyStore interface {
	LoadMWISubscriptionPolicy(iccid, identity string) (mode string, rejectedStatus int, err error)
	SaveMWISubscriptionRejection(iccid, identity string, status int) error
}

// SIPResultStore is an optional delivery-store capability used to preserve
// the initial SIP MESSAGE result while the part is still waiting for RP-ACK.
type SIPResultStore interface {
	MarkSMSDeliveryPartSIPResult(messageID string, partNo, sipCode int, state, errText string, at time.Time) error
}

type DeliveryRecord = smsdelivery.DeliveryRecord
type InboundFragmentOwner = smsdelivery.InboundFragmentOwner
type InboundFragmentScope = smsdelivery.InboundFragmentScope
type InboundFragment = smsdelivery.InboundFragment
type StoredInboundFragment = smsdelivery.StoredInboundFragment
type InboundFragmentSaveResult = smsdelivery.InboundFragmentSaveResult

var ErrInboundFragmentCollision = smsdelivery.ErrInboundFragmentCollision

// InboundFragmentStore optionally persists incomplete inbound multipart SMS.
type InboundFragmentStore = smsdelivery.InboundFragmentStore

// InboundFragmentLifecycleStore optionally persists the degraded notification state.
type InboundFragmentLifecycleStore = smsdelivery.InboundFragmentLifecycleStore

// ServiceStatus is the IMS service registration status.
type ServiceStatus struct {
	Enabled          bool
	DeviceID         string
	Registered       bool
	RegStatus        string
	Registrar        string
	LocalAddr        string
	AssociatedMSISDN string
	LastSIPCode      int
	LastSIPText      string
	PingFailCount    int
	LastSMSAt        time.Time
	LastSMSError     string

	// State and RegState preserve the current status projection.
	State    string
	RegState string
}

// IsRegistered reports whether the service is registered.
func (s ServiceStatus) IsRegistered() bool {
	return s.Registered || strings.EqualFold(strings.TrimSpace(s.RegStatus), "registered")
}

// Service is the original IMS messaging lifecycle surface.
type Service interface {
	CancelUSSD(context.Context, string) error
	ContinueUSSD(context.Context, string, string) (*USSDResult, error)
	GetSMSDeliveryStatus(string) (*DeliveryStatus, error)
	SendSMSWithOptions(context.Context, string, string, SendOptions) (SendOutcome, error)
	SendSMSWithResult(context.Context, string, string) (SendOutcome, error)
	SendUSSD(context.Context, string) (*USSDResult, error)
	Status() map[string]interface{}
	StatusSnapshot() ServiceStatus
	Stop(context.Context) error
	TriggerRegisterImmediate(string)
}
