package phone

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
	"github.com/pion/webrtc/v4"
	"github.com/yibaiba/hideck/pkg/logger"
)

const defaultRecoveryGrace = 15 * time.Second

type Service struct {
	gateway             VoiceGateway
	store               RecordStore
	transcoder          AudioTranscoder
	promptDecoder       AudioPromptDecoder
	notifier            ResultNotifier
	media               *MediaManager
	events              *eventHub
	recordingDir        string
	resolveICCID        func(string) string
	recoveryGrace       time.Duration
	ctx                 context.Context
	cancel              context.CancelFunc
	unsubscribeIncoming func()
	unsubscribeEvents   func()
	mu                  sync.RWMutex
	calls               map[string]*activeCall
	deviceCalls         map[string]string
	mediaCalls          map[string]string
	mediaReservations   map[string]*mediaReservation
	pendingMediaDrops   map[string]*pendingMediaDrop
	pendingEvents       map[string][]voicehost.CallEvent
	terminalSeen        map[string]struct{}
	closeOnce           sync.Once
	closeErr            error
	deviceWaiting       map[string]string
	autoAnswer          autoAnswerRuntime
}

func NewService(options ServiceOptions) (*Service, error) {
	if options.Gateway == nil {
		return nil, errors.New("phone: voice gateway is required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{
		gateway: options.Gateway, store: options.Store, transcoder: options.Transcoder,
		promptDecoder: options.PromptDecoder,
		notifier:      options.Notifier, events: newEventHub(), recordingDir: options.RecordingDir,
		resolveICCID: options.ResolveICCID, recoveryGrace: options.RecoveryGrace,
		ctx: ctx, cancel: cancel, calls: make(map[string]*activeCall),
		deviceCalls: make(map[string]string), mediaCalls: make(map[string]string),
		mediaReservations: make(map[string]*mediaReservation),
		pendingMediaDrops: make(map[string]*pendingMediaDrop),
		pendingEvents:     make(map[string][]voicehost.CallEvent), terminalSeen: make(map[string]struct{}),
		deviceWaiting: make(map[string]string),
	}
	if service.recoveryGrace <= 0 {
		service.recoveryGrace = defaultRecoveryGrace
	}
	media, err := NewMediaManager(MediaOptions{
		UDPAddress: options.WebRTCUDPAddress, PublicHost: options.WebRTCPublicHost,
		ICEServers:     buildICEServers(options.ICEServers),
		RealtimeCodecs: options.RealtimeCodecs, NewRealtimeCodec: options.NewRealtimeCodec,
		OnState: service.handleMediaState,
	})
	if err != nil {
		cancel()
		return nil, err
	}
	service.media = media
	if err := service.configureAutoAnswer(options.AutoAnswer); err != nil {
		cancel()
		_ = media.Close()
		return nil, err
	}
	service.unsubscribeIncoming = options.Gateway.SubscribeIncomingCalls(service.handleIncoming)
	service.unsubscribeEvents = options.Gateway.SubscribeCallEvents(service.handleCallEvent)
	if service.store != nil {
		if err := service.store.AbandonIncomplete(ctx, time.Now(), "process_restart"); err != nil {
			logger.Error("清理未结束通话记录失败", "err", err)
		}
	}
	return service, nil
}

func buildICEServers(urls []string) []webrtc.ICEServer {
	servers := make([]webrtc.ICEServer, 0, len(urls))
	for _, value := range urls {
		if value = strings.TrimSpace(value); value != "" {
			servers = append(servers, webrtc.ICEServer{URLs: []string{value}})
		}
	}
	return servers
}

func (s *Service) CreateMedia(ctx context.Context, owner, offer, accessHost string) (MediaAnswer, error) {
	if strings.TrimSpace(owner) == "" {
		return MediaAnswer{}, errors.New("phone: authenticated owner is required")
	}
	if strings.TrimSpace(offer) == "" {
		return MediaAnswer{}, errors.New("phone: WebRTC SDP offer is required")
	}
	return s.media.Create(ctx, owner, offer, accessHost)
}

func (s *Service) CancelMedia(owner, mediaID, lease string) error {
	mediaID = strings.TrimSpace(mediaID)
	s.mu.Lock()
	media := s.media.Get(mediaID)
	if media == nil {
		s.mu.Unlock()
		return errors.New("phone: media session not found")
	}
	if !media.Matches(owner, strings.TrimSpace(lease)) {
		s.mu.Unlock()
		return errors.New("phone: invalid media control lease")
	}
	if s.mediaCalls[mediaID] != "" || s.mediaReservations[mediaID] != nil {
		s.mu.Unlock()
		return errors.New("phone: media session already has an active call operation")
	}
	media, err := s.media.detachOwned(mediaID, owner, lease)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.clearPendingMediaDropLocked(mediaID)
	s.mu.Unlock()
	return media.Close()
}

func (s *Service) Active(lease string) []CallView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]CallView, 0, len(s.calls))
	for _, call := range s.calls {
		if !call.terminal {
			result = append(result, call.snapshot(lease))
		}
	}
	return result
}

func (s *Service) History(ctx context.Context, limit int) ([]CallRecord, error) {
	if s.store == nil {
		return []CallRecord{}, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return s.store.List(ctx, limit)
}

func (s *Service) Subscribe(afterID uint64) ([]Event, <-chan Event, func()) {
	return s.events.subscribe(afterID)
}

func (s *Service) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() { s.closeErr = s.close(ctx) })
	return s.closeErr
}

func (s *Service) close(ctx context.Context) error {
	calls := s.callSnapshotForClose()
	var result error
	for _, call := range calls {
		result = errors.Join(result, s.closeCall(ctx, call))
		if ctx.Err() != nil {
			break
		}
	}
	s.stopAllMixedRecordings(calls)
	if s.unsubscribeIncoming != nil {
		s.unsubscribeIncoming()
	}
	if s.unsubscribeEvents != nil {
		s.unsubscribeEvents()
	}
	s.stopPendingMediaDropTimers()
	s.cancel()
	return errors.Join(result, s.media.Close())
}

func (s *Service) callSnapshotForClose() []*activeCall {
	s.mu.RLock()
	defer s.mu.RUnlock()
	calls := make([]*activeCall, 0, len(s.calls))
	for _, call := range s.calls {
		if !call.terminal {
			calls = append(calls, call)
		}
	}
	return calls
}

func (s *Service) closeCall(ctx context.Context, call *activeCall) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s.mu.RLock()
	deviceID, callID := call.view.DeviceID, call.view.CallID
	terminalDone, finalizedDone := call.terminalDone, call.finalizedDone
	s.mu.RUnlock()
	result := s.gateway.HangupCall(ctx, deviceID, callID)
	s.finishCall(voicehost.CallEvent{
		Type: "CallEnded", DeviceID: deviceID, CallID: callID,
		Reason: "service_stop", Time: time.Now(),
	})
	if !waitForCallCleanup(ctx, terminalDone, finalizedDone) {
		result = errors.Join(result, ctx.Err())
	}
	return result
}

func waitForCallCleanup(ctx context.Context, terminalDone, finalizedDone <-chan struct{}) bool {
	for _, done := range []<-chan struct{}{terminalDone, finalizedDone} {
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

func (s *Service) captureBase(deviceID string, at time.Time) string {
	deviceID = sanitizeFilePart(deviceID)
	name := fmt.Sprintf("call_%s_%s", deviceID, at.Format("20060102_150405.000000000"))
	return filepath.Join(s.recordingDir, name)
}

func sanitizeFilePart(value string) string {
	return strings.Map(func(char rune) rune {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' {
			return char
		}
		return '_'
	}, strings.TrimSpace(value))
}

func (s *Service) persist(record CallRecord) {
	if s.store == nil {
		return
	}
	if err := s.store.Upsert(s.ctx, record); err != nil {
		logger.Error("电话记录持久化失败", "device_id", record.DeviceID, "call_id", record.CallID, "err", err)
	}
}

func (s *Service) publish(kind string, call *activeCall) {
	s.mu.RLock()
	// SSE is shared by all authenticated browsers, so it must never confer the
	// controller's lease-specific writable view. Clients match their media ID.
	view := call.snapshot("")
	s.mu.RUnlock()
	s.events.publish(Event{Type: kind, Call: view, Time: time.Now()})
}

func (s *Service) callView(callID, lease string) CallView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	call := s.calls[callID]
	if call == nil {
		return CallView{}
	}
	return call.snapshot(lease)
}
