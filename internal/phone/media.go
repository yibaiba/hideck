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
	mux := &physicalMux{UDPMux: webrtc.NewICEUDPMux(nil, connection), allowed: physicalInterfaceIPs()}
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

// physicalMux 把 UDPMux 上报的监听地址收敛到物理网卡。
//
// pion 在监听未指定地址（:7580）时会自己枚举全部网卡，docker 网桥
// （172.x）、模组 wwan（10.126.x）都会变成 host 候选。answer 侧按访问地址
// 裁剪只影响发给浏览器的 SDP，管不到 ICE 内部的候选集——实测 ICE 曾把
// local candidate 选成 10.126.126.2（wwan）和 172.17.0.1（docker），媒体
// 随后 rollover、上行音视频都断掉。
type physicalMux struct {
	ice.UDPMux
	allowed map[string]struct{}
}

func (m *physicalMux) GetListenAddresses() []net.Addr {
	addrs := m.UDPMux.GetListenAddresses()
	if len(m.allowed) == 0 {
		return addrs
	}
	kept := make([]net.Addr, 0, len(addrs))
	for _, addr := range addrs {
		host, _, err := net.SplitHostPort(addr.String())
		if err != nil {
			continue
		}
		if ip, ok := normalizeIPAddress(host); ok {
			if _, allowed := m.allowed[ip]; allowed {
				kept = append(kept, addr)
			}
		}
	}
	if len(kept) == 0 {
		// 过滤后一个都不剩（比如容器里没有物理网卡）时退回原始列表：
		// 宁可多宣告候选，也不能让 ICE 无候选可用。
		return addrs
	}
	return kept
}

// physicalInterfaceIPs 收集物理网卡上的全部地址（IPv4 与 IPv6 都保留，
// 公网 IPv6 接入依赖它们），跳过 docker/veth/wwan 等虚拟接口。
func physicalInterfaceIPs() map[string]struct{} {
	out := make(map[string]struct{})
	interfaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		name := strings.ToLower(iface.Name)
		virtual := false
		for _, prefix := range []string{"docker", "br-", "veth", "tun", "tap", "wg", "wwan", "cni"} {
			if strings.HasPrefix(name, prefix) {
				virtual = true
				break
			}
		}
		if virtual {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			if normalized, ok := normalizeIPAddress(ipNet.IP.String()); ok {
				out[normalized] = struct{}{}
			}
		}
	}
	return out
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

