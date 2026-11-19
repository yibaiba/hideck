package balance

import (
	"context"
	"errors"
	"time"

	"github.com/yibaiba/hideck/internal/carrierquery"
)

const DefaultQueryTimeout = 5 * time.Minute

const (
	StateSending       = "sending"
	StateAwaitingReply = "awaiting_reply"
	StateCompleted     = "completed"
	StateTimedOut      = "timed_out"
	StateFailed        = "failed"
)

const (
	ParsePending  = "pending"
	ParseParsed   = "parsed"
	ParseUnparsed = "unparsed"
	ParseManual   = "manual"
)

const TransportManual = "manual"

var (
	ErrDeviceNotFound  = errors.New("设备未找到")
	ErrIdentityMissing = errors.New("SIM 身份未就绪")
	ErrRuleNotFound    = errors.New("未找到运营商余额查询规则")
	ErrUnsupported     = errors.New("当前运营商不支持自动余额查询")
	ErrPendingQuery    = errors.New("该 SIM 已有待处理的余额查询")
	ErrInvalidManual   = errors.New("手动余额格式无效")
)

type DeviceSnapshot struct {
	DeviceID          string `json:"device_id"`
	ICCID             string `json:"iccid"`
	MCC               string `json:"mcc"`
	MNC               string `json:"mnc"`
	SPN               string `json:"spn,omitempty"`
	VoWiFiActive      bool   `json:"vowifi_active"`
	RouteSMSViaVoWiFi bool   `json:"-"`
}

type USSDResponse struct {
	Text string
	Raw  string
}

type InboundSMS struct {
	DeviceID string
	ICCID    string
	Sender   string
	Content  string
	Time     time.Time
}

type Query struct {
	ID            string     `json:"id"`
	DeviceID      string     `json:"device_id"`
	ICCID         string     `json:"iccid"`
	RuleID        string     `json:"rule_id"`
	Transport     string     `json:"transport"`
	State         string     `json:"state"`
	ParseState    string     `json:"parse_state"`
	Amount        string     `json:"amount,omitempty"`
	Currency      string     `json:"currency,omitempty"`
	Summary       string     `json:"summary,omitempty"`
	RawResponse   string     `json:"raw_response,omitempty"`
	ResponseSMSID *uint      `json:"response_sms_id,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
	Error         string     `json:"error,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type Completion struct {
	ParseState  string
	Amount      string
	Currency    string
	Summary     string
	RawResponse string
	SMSID       *uint
}

type Gateway interface {
	Snapshot(deviceID string) (DeviceSnapshot, error)
	SendVoWiFiSMS(context.Context, DeviceSnapshot, string, string) error
	SendBackendSMS(context.Context, DeviceSnapshot, string, string) error
	SendVoWiFiUSSD(context.Context, string, string) (USSDResponse, error)
	SendBackendUSSD(context.Context, string, string) (USSDResponse, error)
}

type Repository interface {
	CreatePending(context.Context, Query) error
	SaveManual(context.Context, Query) error
	DeleteManual(context.Context, string) (bool, error)
	MarkAwaitingReply(context.Context, string, time.Time) error
	MarkFailed(context.Context, string, error, time.Time) error
	Complete(context.Context, string, Completion, time.Time) (bool, error)
	FindPending(context.Context, string, time.Time) (Query, bool, error)
	ExpirePending(context.Context, time.Time) (int64, error)
	Get(context.Context, string) (Query, bool, error)
	List(context.Context, string, int, *time.Time) ([]Query, error)
}

type RuleResolver interface {
	Resolve(context.Context, DeviceSnapshot) (carrierquery.Rule, error)
	ByID(context.Context, string) (carrierquery.Rule, error)
}
