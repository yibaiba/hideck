package volte

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
)

type callMedia struct {
	// The bridge pointer stays immutable; Close and SetRemote synchronize inside it.
	bridge *PCMBridge
	conn   net.PacketConn
	sdp    string
	pcm    PCMPort
}

func pcmuOfferSDP(port int) string {
	return fmt.Sprintf(
		"v=0\r\no=hideck 0 0 IN IP4 127.0.0.1\r\ns=HiDeck VoLTE\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio %d RTP/AVP 0 101\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:101 telephone-event/8000\r\na=fmtp:101 0-15\r\na=ptime:20\r\na=sendrecv\r\n",
		port,
	)
}

func rtpPortFromSDP(sdp string) (int, error) {
	for _, raw := range strings.Split(strings.ReplaceAll(sdp, "\r\n", "\n"), "\n") {
		fields := strings.Fields(strings.TrimSpace(raw))
		if len(fields) >= 2 && fields[0] == "m=audio" {
			port, err := strconv.Atoi(fields[1])
			if err != nil || port <= 0 || port > 65535 {
				return 0, fmt.Errorf("volte: SDP audio port %q", fields[1])
			}
			return port, nil
		}
	}
	return 0, fmt.Errorf("volte: SDP has no m=audio")
}

func sdpHasRecvOnly(sdp string) bool {
	return strings.Contains(sdp, "a=recvonly")
}

type nullPCM struct{}

func (nullPCM) ReadFrame() ([]int16, error) {
	return make([]int16, pcmuFrameSamples), nil
}
func (nullPCM) WriteFrame([]int16) error { return nil }
func (nullPCM) Close() error             { return nil }

func startCallMedia(browserSDP string, pcm PCMPort, listenOnly bool) (*callMedia, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		return nil, fmt.Errorf("volte: listen PCMU: %w", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	var remote net.Addr
	if browserPort, err := rtpPortFromSDP(browserSDP); err == nil && browserPort > 0 {
		remote = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: browserPort}
	}
	if pcm == nil {
		pcm = nullPCM{}
	}
	if sdpHasRecvOnly(browserSDP) {
		listenOnly = true
	}
	bridge := NewPCMBridge(conn, remote, pcm, listenOnly)
	return &callMedia{bridge: bridge, conn: conn, sdp: pcmuOfferSDP(port), pcm: pcm}, nil
}

func (m *callMedia) Close() error {
	if m == nil {
		return nil
	}
	var err error
	if m.bridge != nil {
		err = m.bridge.Close()
	}
	return err
}

type mediaTable struct {
	mu    sync.Mutex
	items map[string]*callMedia
}

func (t *mediaTable) put(id string, media *callMedia) {
	t.mu.Lock()
	if t.items == nil {
		t.items = map[string]*callMedia{}
	}
	if old := t.items[id]; old != nil {
		_ = old.Close()
	}
	t.items[id] = media
	t.mu.Unlock()
}

func (t *mediaTable) take(id string) *callMedia {
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.items[id]
	delete(t.items, id)
	return m
}

func (t *mediaTable) get(id string) *callMedia {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.items[id]
}
