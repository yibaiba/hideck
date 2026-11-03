package phone

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
)

var ErrActiveCallHistoryDelete = errors.New("正在进行的通话记录不能删除")

const (
	StatusCalling   = "calling"
	StatusRinging   = "ringing"
	StatusWaiting   = "waiting"
	StatusConnected = "connected"
	StatusCompleted = "completed"
	StatusMissed    = "missed"
	StatusRejected  = "rejected"
	StatusBusy      = "busy"
	StatusFailed    = "failed"
)

type VoiceGateway interface {
	SubscribeIncomingCalls(func(voicehost.IncomingCall)) func()
	SubscribeCallEvents(func(voicehost.CallEvent)) func()
	// A nonempty snapshot with an error retains an uncertain dial for control;
	// the error must still be exposed, not treated as successful dialing.
	BeginCall(context.Context, voicehost.BeginCallRequest) (voicehost.CallSnapshot, error)
	ActiveCall(deviceID string) *voicehost.CallSnapshot
	AnswerIncomingCall(context.Context, voicehost.AnswerRequest) (voicehost.AnswerResult, error)
	RejectIncomingCall(voicehost.RejectRequest) error
	HangupCall(context.Context, string, string) error
	SendCallDTMF(string, string, string) error
	HoldCall(context.Context, string, string) error
	ResumeCall(context.Context, string, string) error
	SwitchCall(string, string) error
	StartCallCapture(string, string, string) error
}

type RecordStore interface {
	Upsert(context.Context, CallRecord) error
	List(context.Context, int) ([]CallRecord, error)
	Delete(context.Context, []uint64) ([]CallRecord, error)
	AbandonIncomplete(ctx context.Context, endedAt time.Time, reason string) error
}

type AudioTranscoder interface {
	ToMP3(context.Context, string) (string, error)
}

type AudioPromptDecoder interface {
	DecodePCM(context.Context, string) ([]int16, int, error)
}

type AutoAnswerConfig struct {
	AudioPath string
	Devices   map[string]AutoAnswerDeviceConfig
}

type AutoAnswerDeviceConfig struct {
	Enabled bool
	Delay   time.Duration
}

type AutoAnswerStatus struct {
	AudioPath string
	Devices   map[string]AutoAnswerDeviceConfig
}

type RealtimeCodec interface {
	SampleRate() int
	Decode(payload []byte) ([]int16, error)
	Encode(pcm []int16) ([]byte, error)
	Close() error
}

type RealtimeCodecFactory func(codec, fmtp string) (RealtimeCodec, error)

type ResultNotifier interface {
	NotifyIncomingCall(deviceID, caller, callee string)
	NotifyCallResult(deviceID, peer, direction, status, reason string, at time.Time)
}

type AutoAnswerRecordingNotification struct {
	DeviceID      string
	Peer          string
	RecordingPath string
	RecordingName string
	At            time.Time
}

type autoAnswerRecordingNotifier interface {
	NotifyAutoAnswerRecording(notification AutoAnswerRecordingNotification)
}

type ServiceOptions struct {
	Gateway          VoiceGateway
	Store            RecordStore
	Transcoder       AudioTranscoder
	PromptDecoder    AudioPromptDecoder
	Notifier         ResultNotifier
	RecordingDir     string
	WebRTCUDPAddress string
	WebRTCPublicHost string
	ICEServers       []string
	RealtimeCodecs   []string
	NewRealtimeCodec RealtimeCodecFactory
	RecoveryGrace    time.Duration
	ResolveICCID     func(string) string
	AutoAnswer       AutoAnswerConfig
}

type CallRecord struct {
	ID              uint64     `json:"id"`
	CallID          string     `json:"call_id"`
	DeviceID        string     `json:"device_id"`
	ICCID           string     `json:"iccid,omitempty"`
	Direction       string     `json:"direction"`
	Peer            string     `json:"peer"`
	Status          string     `json:"status"`
	StartedAt       time.Time  `json:"started_at"`
	AnsweredAt      *time.Time `json:"answered_at,omitempty"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
	DurationSeconds int64      `json:"duration_seconds"`
	EndReason       string     `json:"end_reason,omitempty"`
	Codec           string     `json:"codec,omitempty"`
	RecordingName   string     `json:"recording_name,omitempty"`
	PCAPName        string     `json:"pcap_name,omitempty"`
	RecordingError  string     `json:"recording_error,omitempty"`
}

type CallView struct {
	CallID         string     `json:"call_id"`
	DeviceID       string     `json:"device_id"`
	Direction      string     `json:"direction"`
	Peer           string     `json:"peer"`
	Status         string     `json:"status"`
	MediaID        string     `json:"media_id,omitempty"`
	StartedAt      time.Time  `json:"started_at"`
	AnsweredAt     *time.Time `json:"answered_at,omitempty"`
	EndedAt        *time.Time `json:"ended_at,omitempty"`
	EndReason      string     `json:"end_reason,omitempty"`
	Codec          string     `json:"codec,omitempty"`
	RecordingError string     `json:"recording_error,omitempty"`
	ReadOnly       bool       `json:"read_only"`
	Held           bool       `json:"held"`
}

type StartCallRequest struct {
	Owner    string
	DeviceID string
	Callee   string
	MediaID  string
	Lease    string
}

type ControlRequest struct {
	Owner   string
	CallID  string
	MediaID string
	Lease   string
}

type RefreshRequest struct {
	Owner    string
	CallID   string
	MediaID  string
	Lease    string
	Takeover bool
}

type Event struct {
	ID   uint64    `json:"id"`
	Type string    `json:"type"`
	Call CallView  `json:"call"`
	Time time.Time `json:"time"`
}

type activeCall struct {
	mediaRefresh    sync.Mutex
	view            CallView
	record          CallRecord
	owner           string
	lease           string
	mediaID         string
	incomingSDP     string
	recordingBase   string
	userRejected    bool
	terminal        bool
	disconnectTimer *time.Timer
	mixedRecorder   *mixedRecorder
	mixedAudioPath  string
	mixedRecordOnce sync.Once
	mixedAttempted  bool
	autoAnswered    bool
	autoAnswerTimer *time.Timer
	controlPending  bool
	terminalDone    chan struct{}
	terminalOnce    sync.Once
	finalizedDone   chan struct{}
	finalizedOnce   sync.Once
}

func (call *activeCall) snapshot(lease string) CallView {
	view := call.view
	view.ReadOnly = call.lease != "" && call.lease != lease
	if call.lease == "" && view.Status == StatusConnected {
		view.ReadOnly = true
	}
	return view
}
