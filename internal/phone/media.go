package phone

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

const (
	pcmPayloadType = 0
)

type MediaOptions struct {
	UDPAddress       string
	PublicHost       string
	ICEServers       []webrtc.ICEServer
	RealtimeCodecs   []string
	NewRealtimeCodec RealtimeCodecFactory
	OnState          func(mediaID string, state webrtc.PeerConnectionState)
}

type MediaAnswer struct {
	MediaID string `json:"media_id"`
	Lease   string `json:"lease"`
	SDP     string `json:"sdp"`
}

type MediaStats struct {
	PacketsFromIMS uint64 `json:"packets_from_ims"`
	PacketsToIMS   uint64 `json:"packets_to_ims"`
	PacketsLost    uint64 `json:"packets_lost"`
}

type MediaManager struct {
	api              *webrtc.API
	mux              ice.UDPMux
	iceServers       []webrtc.ICEServer
	realtimeCodecs   []string
	newRealtimeCodec RealtimeCodecFactory
	onState          func(string, webrtc.PeerConnectionState)
	mu               sync.RWMutex
	sessions         map[string]*MediaSession
}

func NewMediaManager(options MediaOptions) (*MediaManager, error) {
	address := options.UDPAddress
	if address == "" {
		address = ":7580"
	}
	udpAddress, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, fmt.Errorf("phone: resolve WebRTC UDP address: %w", err)
	}
	connection, err := net.ListenUDP("udp", udpAddress)
	if err != nil {
		return nil, fmt.Errorf("phone: listen WebRTC UDP mux on %s: %w", address, err)
	}
	mux := webrtc.NewICEUDPMux(nil, connection)
	api, err := newWebRTCAPI(mux, options.PublicHost)
	if err != nil {
		_ = mux.Close()
		return nil, err
	}
	return &MediaManager{
		api: api, mux: mux, iceServers: options.ICEServers,
		realtimeCodecs:   append([]string(nil), options.RealtimeCodecs...),
		newRealtimeCodec: options.NewRealtimeCodec,
		onState:          options.OnState, sessions: make(map[string]*MediaSession),
	}, nil
}

func newWebRTCAPI(mux ice.UDPMux, publicHost string) (*webrtc.API, error) {
	mediaEngine := &webrtc.MediaEngine{}
	err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1,
		},
		PayloadType: pcmPayloadType,
	}, webrtc.RTPCodecTypeAudio)
	if err != nil {
		return nil, fmt.Errorf("phone: register PCMU codec: %w", err)
	}
	settings := webrtc.SettingEngine{}
	settings.SetICEUDPMux(mux)
	if err := setWebRTCPublicHost(&settings, publicHost, net.DefaultResolver.LookupIPAddr); err != nil {
		return nil, err
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithSettingEngine(settings)), nil
}

type publicIPLookup func(context.Context, string) ([]net.IPAddr, error)

func setWebRTCPublicHost(settings *webrtc.SettingEngine, value string, lookup publicIPLookup) error {
	publicHost := strings.TrimSpace(value)
	if publicHost == "" {
		return nil
	}
	publicIPs, err := resolveWebRTCPublicIPs(publicHost, lookup)
	if err != nil {
		return err
	}
	if err := settings.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
		External:        publicIPs,
		AsCandidateType: webrtc.ICECandidateTypeHost,
		Mode:            webrtc.ICEAddressRewriteAppend,
	}); err != nil {
		return fmt.Errorf("phone: configure WebRTC public host: %w", err)
	}
	return nil
}

func resolveWebRTCPublicIPs(host string, lookup publicIPLookup) ([]string, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []string{ip.String()}, nil
	}
	addresses, err := lookup(context.Background(), host)
	if err != nil {
		return nil, fmt.Errorf("phone: resolve WebRTC public host %q: %w", host, err)
	}
	unique := make(map[string]struct{}, len(addresses))
	result := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if address.IP == nil {
			continue
		}
		ip := address.IP.String()
		if _, exists := unique[ip]; exists {
			continue
		}
		unique[ip] = struct{}{}
		result = append(result, ip)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("phone: WebRTC public host %q resolved without IP addresses", host)
	}
	return result, nil
}

func (m *MediaManager) Create(ctx context.Context, owner, offer, accessHost string) (MediaAnswer, error) {
	if m == nil || m.api == nil {
		return MediaAnswer{}, errors.New("phone: media manager is unavailable")
	}
	mediaID, err := randomToken(16)
	if err != nil {
		return MediaAnswer{}, err
	}
	lease, err := randomToken(32)
	if err != nil {
		return MediaAnswer{}, err
	}
	session, answer, err := newMediaSession(ctx, mediaSessionOptions{
		ID: mediaID, Lease: lease, Owner: owner, Offer: offer, AccessHost: accessHost,
		API: m.api, ICEServers: m.iceServers, RealtimeCodecs: m.realtimeCodecs,
		NewRealtimeCodec: m.newRealtimeCodec, OnState: m.onState,
	})
	if err != nil {
		return MediaAnswer{}, err
	}
	m.mu.Lock()
	m.sessions[mediaID] = session
	m.mu.Unlock()
	return MediaAnswer{MediaID: mediaID, Lease: lease, SDP: answer}, nil
}

func (m *MediaManager) CreateHeadless(owner string) (*MediaSession, error) {
	if m == nil {
		return nil, errors.New("phone: media manager is unavailable")
	}
	mediaID, err := randomToken(16)
	if err != nil {
		return nil, err
	}
	session, err := newHeadlessMediaSession(mediaSessionOptions{
		ID: mediaID, Owner: owner, RealtimeCodecs: m.realtimeCodecs,
		NewRealtimeCodec: m.newRealtimeCodec,
	})
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.sessions[mediaID] = session
	m.mu.Unlock()
	return session, nil
}

func (m *MediaManager) Get(mediaID string) *MediaSession {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[mediaID]
}

func (m *MediaManager) Remove(mediaID string) {
	session := m.detach(mediaID)
	if session != nil {
		_ = session.Close()
	}
}

func (m *MediaManager) detach(mediaID string) *MediaSession {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[mediaID]
	delete(m.sessions, mediaID)
	return session
}

func (m *MediaManager) detachOwned(mediaID, owner, lease string) (*MediaSession, error) {
	if m == nil {
		return nil, errors.New("phone: media manager is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[mediaID]
	if session == nil {
		return nil, errors.New("phone: media session not found")
	}
	if !session.Matches(owner, strings.TrimSpace(lease)) {
		return nil, errors.New("phone: invalid media control lease")
	}
	delete(m.sessions, mediaID)
	return session, nil
}

func (m *MediaManager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	sessions := make([]*MediaSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.sessions = make(map[string]*MediaSession)
	m.mu.Unlock()
	var result error
	for _, session := range sessions {
		result = errors.Join(result, session.Close())
	}
	return errors.Join(result, m.mux.Close())
}

func randomToken(bytes int) (string, error) {
	buffer := make([]byte, bytes)
	if _, err := io.ReadFull(rand.Reader, buffer); err != nil {
		return "", fmt.Errorf("phone: generate secure token: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}
