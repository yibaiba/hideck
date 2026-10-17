package phone

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/yibaiba/hideck/pkg/logger"
)

type mediaSessionOptions struct {
	ID, Lease, Owner, Offer string
	API                     *webrtc.API
	ICEServers              []webrtc.ICEServer
	RealtimeCodecs          []string
	NewRealtimeCodec        RealtimeCodecFactory
	OnState                 func(string, webrtc.PeerConnectionState)
	// AccessHost 是浏览器访问管理页时用的 Host（IP 或域名，含端口）。
	// 媒体候选按它裁剪：局域网访问只宣告 LAN 地址，公网 IPv6 访问只宣告
	// 该 IPv6，域名则用解析结果。零值表示不裁剪。
	AccessHost string
}

type MediaSession struct {
	ID, Lease, Owner string
	peer             *webrtc.PeerConnection
	track            *webrtc.TrackLocalStaticRTP
	rtpConn          *net.UDPConn
	onState          func(string, webrtc.PeerConnectionState)
	realtimeCodecs   []string
	newRealtimeCodec RealtimeCodecFactory
	mu               sync.RWMutex
	remote           rtpEndpoint
	realtimeCodec    RealtimeCodec
	recorder         *mixedRecorder
	attached         bool
	closed           chan struct{}
	closeOnce        sync.Once
	silentWorker     sync.WaitGroup
	silentStarted    bool
	receiveOnly      bool
	accessHost       string
	fromIMS          atomic.Uint64
	toIMS            atomic.Uint64
	lost             atomic.Uint64
	browserPackets   atomic.Uint64
	browserRejected  atomic.Uint64
	browserPeak      atomic.Uint64
}

func newMediaSession(ctx context.Context, options mediaSessionOptions) (*MediaSession, string, error) {
	receiveOnly, err := browserOfferReceivesOnlyAudio(options.Offer)
	if err != nil {
		return nil, "", err
	}
	connection, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, "", fmt.Errorf("phone: listen RTP bridge: %w", err)
	}
	peer, err := options.API.NewPeerConnection(webrtc.Configuration{ICEServers: options.ICEServers})
	if err != nil {
		_ = connection.Close()
		return nil, "", fmt.Errorf("phone: create PeerConnection: %w", err)
	}
	session := &MediaSession{
		ID: options.ID, Lease: options.Lease, Owner: options.Owner,
		peer: peer, rtpConn: connection, onState: options.OnState,
		realtimeCodecs:   append([]string(nil), options.RealtimeCodecs...),
		newRealtimeCodec: options.NewRealtimeCodec, receiveOnly: receiveOnly, accessHost: options.AccessHost,
		closed: make(chan struct{}),
	}
	answer, err := session.negotiate(ctx, options.Offer)
	if err != nil {
		_ = session.Close()
		return nil, "", err
	}
	go session.forwardIMSRTP()
	return session, answer, nil
}

func newHeadlessMediaSession(options mediaSessionOptions) (*MediaSession, error) {
	connection, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, fmt.Errorf("phone: listen auto-answer RTP bridge: %w", err)
	}
	session := &MediaSession{
		ID: options.ID, Owner: options.Owner, rtpConn: connection,
		realtimeCodecs:   append([]string(nil), options.RealtimeCodecs...),
		newRealtimeCodec: options.NewRealtimeCodec, closed: make(chan struct{}),
	}
	go session.forwardIMSRTP()
	return session, nil
}

func (s *MediaSession) negotiate(ctx context.Context, offer string) (string, error) {
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1,
	}, "audio", "hideck-phone")
	if err != nil {
		return "", fmt.Errorf("phone: create browser audio track: %w", err)
	}
	s.track = track
	sender, err := s.peer.AddTrack(track)
	if err != nil {
		return "", fmt.Errorf("phone: add browser audio track: %w", err)
	}
	go drainRTCP(sender, s.closed)
	s.peer.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) { go s.forwardBrowserRTP(remote) })
	s.peer.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if s.onState != nil {
			s.onState(s.ID, state)
		}
	})
	if err := s.peer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		return "", fmt.Errorf("phone: apply WebRTC offer: %w", err)
	}
	gatheringComplete := webrtc.GatheringCompletePromise(s.peer)
	answer, err := s.peer.CreateAnswer(nil)
	if err != nil {
		return "", fmt.Errorf("phone: create WebRTC answer: %w", err)
	}
	if err := s.peer.SetLocalDescription(answer); err != nil {
		return "", fmt.Errorf("phone: apply WebRTC answer: %w", err)
	}
	select {
	case <-gatheringComplete:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	sdp := s.peer.LocalDescription().SDP
	// 候选按浏览器实际访问的地址裁剪：多网卡/多地址族同时宣告会让 ICE 先
	// 检查不可达的地址（实测公网 IPv6 对把同网段通话拖到 9 秒才连通）。
	if allowed := resolveAccessHosts(ctx, s.accessHost); len(allowed) > 0 {
		if pruned, kept := pruneAnswerCandidates(sdp, allowed); kept > 0 {
			sdp = pruned
			logger.Info("浏览器媒体候选裁剪", "media_id", s.ID, "access_host", s.accessHost,
				"allowed", allowed, "kept", kept)
		} else {
			logger.Warn("访问地址不在本机候选内，保留全部候选", "media_id", s.ID,
				"access_host", s.accessHost, "allowed", allowed)
		}
	}
	return sdp, nil
}

func (s *MediaSession) PlainSDP() string {
	port := s.rtpConn.LocalAddr().(*net.UDPAddr).Port
	s.mu.RLock()
	endpoint, attached := s.remote, s.attached
	s.mu.RUnlock()
	if attached {
		return plainSelectedAudioSDP(port, endpoint)
	}
	return plainAudioSDP(port, s.realtimeCodecs)
}

func (s *MediaSession) Attach(remoteSDP string) error {
	supported := append([]string{"PCMU", "PCMA"}, s.realtimeCodecs...)
	endpoint, err := parseRTPEndpoint(remoteSDP, supported...)
	if err != nil {
		return err
	}
	codec, err := s.createRealtimeCodec(endpoint)
	if err != nil {
		return err
	}
	s.mu.Lock()
	previous := s.realtimeCodec
	s.realtimeCodec = codec
	s.remote, s.attached = endpoint, true
	s.mu.Unlock()
	var closeErr error
	if previous != nil {
		closeErr = previous.Close()
	}
	primeErr := s.primeRelay()
	if primeErr == nil && s.receiveOnly {
		s.startSilentRTP()
	}
	return errors.Join(closeErr, primeErr)
}

func (s *MediaSession) createRealtimeCodec(endpoint rtpEndpoint) (RealtimeCodec, error) {
	if endpoint.Codec != "AMR" && endpoint.Codec != "AMR-WB" && endpoint.Codec != "EVS" {
		return nil, nil
	}
	if s.newRealtimeCodec == nil {
		return nil, fmt.Errorf("phone: negotiated %s codec requires an unavailable encoder", endpoint.Codec)
	}
	codec, err := s.newRealtimeCodec(endpoint.Codec, endpoint.Fmtp)
	if err != nil {
		return nil, fmt.Errorf("phone: initialize negotiated %s codec: %w", endpoint.Codec, err)
	}
	if codec == nil {
		return nil, fmt.Errorf("phone: initialize negotiated %s codec: factory returned nil", endpoint.Codec)
	}
	if codec.SampleRate() != endpoint.ClockRate {
		mismatch := fmt.Errorf("phone: negotiated %s clock rate %d does not match codec rate %d", endpoint.Codec, endpoint.ClockRate, codec.SampleRate())
		return nil, errors.Join(mismatch, codec.Close())
	}
	return codec, nil
}

func (s *MediaSession) Matches(owner, lease string) bool {
	return s != nil && s.Owner == owner && secureEqual(s.Lease, lease)
}

func (s *MediaSession) SetRecorder(recorder *mixedRecorder) {
	s.mu.Lock()
	s.recorder = recorder
	s.mu.Unlock()
}

func (s *MediaSession) Stats() MediaStats {
	return MediaStats{
		PacketsFromIMS: s.fromIMS.Load(), PacketsToIMS: s.toIMS.Load(), PacketsLost: s.lost.Load(),
	}
}

func (s *MediaSession) Close() error {
	if s == nil {
		return nil
	}
	var result error
	s.closeOnce.Do(func() {
		logger.Info("网页音频转发统计", "media_id", s.ID, "browser_packets", s.browserPackets.Load(), "browser_rejected", s.browserRejected.Load(), "browser_peak", s.browserPeak.Load(), "to_modem", s.toIMS.Load(), "from_modem", s.fromIMS.Load(), "lost", s.lost.Load())
		s.stopSilentRTP()
		s.mu.Lock()
		codec := s.realtimeCodec
		s.realtimeCodec = nil
		s.recorder = nil
		s.mu.Unlock()
		if codec != nil {
			result = errors.Join(result, codec.Close())
		}
		if s.peer != nil {
			result = errors.Join(result, s.peer.Close())
		}
		if s.rtpConn != nil {
			result = errors.Join(result, s.rtpConn.Close())
		}
	})
	return result
}

func (s *MediaSession) StartPrompt(pcm []int16, sampleRate int, onError func(error)) {
	copyPCM := append([]int16(nil), pcm...)
	s.mu.Lock()
	if s.silentStarted || sessionClosed(s.closed) {
		s.mu.Unlock()
		return
	}
	s.silentStarted = true
	s.silentWorker.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.silentWorker.Done()
		if err := s.forwardPromptThenSilence(copyPCM, sampleRate); err != nil && onError != nil {
			onError(err)
		}
	}()
}

func (s *MediaSession) forwardPromptThenSilence(pcm []int16, sampleRate int) error {
	pcm, err := resamplePCM(pcm, sampleRate, browserClockRate)
	if err != nil {
		return err
	}
	ticker := time.NewTicker(jitterTick)
	defer ticker.Stop()
	sequence := uint16(1)
	// Attach primes sequence 1 at one frame. Continue from that RTP position.
	timestamp := uint32(browserSamplesPerFrame)
	for offset := 0; ; offset += browserSamplesPerFrame {
		select {
		case <-s.closed:
			return nil
		case <-ticker.C:
		}
		frame := make([]int16, browserSamplesPerFrame)
		if offset < len(pcm) {
			copy(frame, pcm[offset:min(offset+browserSamplesPerFrame, len(pcm))])
		}
		sequence++
		timestamp += browserSamplesPerFrame
		if err := s.writePromptRTP(sequence, timestamp, encodePCMU(frame)); err != nil {
			return err
		}
	}
}

func (s *MediaSession) writePromptRTP(sequence uint16, browserTimestamp uint32, payload []byte) error {
	endpoint, codec, ok := s.endpoint()
	if !ok {
		return errors.New("phone: auto-answer media is not attached")
	}
	s.recordMixedFrame(mixToIMS, payload)
	encoded, err := browserPayloadForIMS(payload, endpoint, codec)
	if err != nil {
		return err
	}
	packet := &rtp.Packet{Header: rtp.Header{
		Version: 2, PayloadType: endpoint.PayloadType, SequenceNumber: sequence,
		Timestamp: scaleTimestamp(browserTimestamp, browserClockRate, endpoint.ClockRate), SSRC: 0x48494445,
	}, Payload: encoded}
	raw, err := packet.Marshal()
	if err != nil {
		return err
	}
	_, err = s.rtpConn.WriteToUDP(raw, endpoint.Address)
	if err == nil {
		s.toIMS.Add(1)
	}
	return err
}

func (s *MediaSession) startSilentRTP() {
	s.mu.Lock()
	if s.silentStarted || sessionClosed(s.closed) {
		s.mu.Unlock()
		return
	}
	s.silentStarted = true
	s.silentWorker.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.silentWorker.Done()
		s.forwardSilentRTP()
	}()
}

func (s *MediaSession) stopSilentRTP() {
	s.mu.Lock()
	close(s.closed)
	s.mu.Unlock()
	s.silentWorker.Wait()
}

func sessionClosed(closed <-chan struct{}) bool {
	select {
	case <-closed:
		return true
	default:
		return false
	}
}

func drainRTCP(sender *webrtc.RTPSender, done <-chan struct{}) {
	buffer := make([]byte, 1500)
	for {
		if _, _, err := sender.Read(buffer); err != nil {
			return
		}
		select {
		case <-done:
			return
		default:
		}
	}
}

// resolveAccessHosts 把浏览器访问用的 Host 变成允许宣告的 IP 列表：
// 带端口的 Host 去掉端口；IP 字面量直接使用；域名按 A/AAAA 解析（2 秒超时）。
func resolveAccessHosts(ctx context.Context, host string) []string {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil
	}
	if trimmed, _, err := net.SplitHostPort(host); err == nil {
		host = trimmed
	}
	host = strings.Trim(host, "[]")
	if ip, ok := normalizeIPAddress(host); ok {
		return []string{ip}
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{}, len(addresses))
	result := make([]string, 0, len(addresses))
	for _, address := range addresses {
		normalized, ok := normalizeIPAddress(address.IP.String())
		if !ok {
			continue
		}
		if _, duplicate := seen[normalized]; duplicate {
			continue
		}
		seen[normalized] = struct{}{}
		result = append(result, normalized)
	}
	return result
}

// normalizeIPAddress 把 IP 规范化（IPv4-mapped IPv6 还原成 IPv4）。
func normalizeIPAddress(value string) (string, bool) {
	address, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil {
		return "", false
	}
	return address.Unmap().String(), true
}

// pruneAnswerCandidates 只保留地址属于 allowed 的候选行，返回新 SDP 与
// 保留的候选数；一条都留不下时返回 0，调用方应保留原始候选。
func pruneAnswerCandidates(sdp string, allowed []string) (string, int) {
	allow := make(map[string]struct{}, len(allowed))
	for _, ip := range allowed {
		if normalized, ok := normalizeIPAddress(ip); ok {
			allow[normalized] = struct{}{}
		}
	}
	if len(allow) == 0 {
		return sdp, 0
	}
	lines := strings.Split(sdp, "\n")
	kept := 0
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "a=candidate:") {
			fields := strings.Fields(strings.TrimPrefix(trimmed, "a=candidate:"))
			if len(fields) >= 6 {
				if _, ok := allow[normalizeCandidateAddress(fields[4])]; !ok {
					continue
				}
				kept++
			}
		}
		out = append(out, line)
	}
	if kept == 0 {
		return sdp, 0
	}
	return strings.Join(out, "\n"), kept
}

func normalizeCandidateAddress(value string) string {
	if normalized, ok := normalizeIPAddress(value); ok {
		return normalized
	}
	return strings.ToLower(strings.TrimSpace(value))
}
