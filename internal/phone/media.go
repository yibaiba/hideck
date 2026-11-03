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
	LookupIPAddr     func(context.Context, string) ([]net.IPAddr, error)
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
	publicHost       *webRTCPublicHost
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
	publicHost := newWebRTCPublicHost(options.PublicHost, options.LookupIPAddr)
	var publicIPs []string
	if publicHost != nil {
		// A host that does not resolve at startup is still a configuration
		// error; startup keeps the resolver's own timeout, as before.
		if publicIPs, err = publicHost.resolve(context.Background()); err != nil {
			_ = mux.Close()
			return nil, err
		}
		if net.ParseIP(publicHost.host) != nil {
			publicHost = nil // a literal never changes; the startup API serves every call
		}
	}
	api, err := newWebRTCAPI(mux, publicIPs)
	if err != nil {
		_ = mux.Close()
		return nil, err
	}
	return &MediaManager{
		api: api, mux: mux, publicHost: publicHost, iceServers: options.ICEServers,
		realtimeCodecs:   append([]string(nil), options.RealtimeCodecs...),
		newRealtimeCodec: options.NewRealtimeCodec,
		onState:          options.OnState, sessions: make(map[string]*MediaSession),
	}, nil
}

func newWebRTCAPI(mux ice.UDPMux, publicIPs []string) (*webrtc.API, error) {
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
	if len(publicIPs) > 0 {
		if err := settings.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
			External:        publicIPs,
			AsCandidateType: webrtc.ICECandidateTypeHost,
			Mode:            webrtc.ICEAddressRewriteAppend,
		}); err != nil {
			return nil, fmt.Errorf("phone: configure WebRTC public host: %w", err)
		}
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithSettingEngine(settings)), nil
}

// callAPI returns the API for one call. With webrtc_public_host set it is built
// from a fresh lookup, so a dynamic-DNS name follows the WAN address after a
// redial instead of keeping the address resolved at startup (hideck#93).
func (m *MediaManager) callAPI(ctx context.Context) (*webrtc.API, error) {
	if m.publicHost == nil {
		return m.api, nil
	}
	publicIPs, err := m.publicHost.resolve(ctx)
	if err != nil {
		return nil, err
	}
	return newWebRTCAPI(m.mux, publicIPs)
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
	api, err := m.callAPI(ctx)
	if err != nil {
		return MediaAnswer{}, err
	}
	session, answer, err := newMediaSession(ctx, mediaSessionOptions{
		ID: mediaID, Lease: lease, Owner: owner, Offer: offer, AccessHost: accessHost,
		API: api, ICEServers: m.iceServers, RealtimeCodecs: m.realtimeCodecs,
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
