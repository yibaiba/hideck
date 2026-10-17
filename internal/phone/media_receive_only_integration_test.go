package phone

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type receiveOnlyTest struct {
	codec       string
	payloadType uint8
	clockRate   int
}

type receiveOnlyFixture struct {
	ims          *net.UDPConn
	session      *MediaSession
	remoteTracks <-chan *webrtc.TrackRemote
	config       receiveOnlyTest
}

func TestReceiveOnlyBrowserGetsAudioAndIMSReceivesContinuousSilence(t *testing.T) {
	for _, test := range []receiveOnlyTest{
		{codec: "PCMU", payloadType: 0, clockRate: 8000},
		{codec: "PCMA", payloadType: 8, clockRate: 8000},
		{codec: "AMR", payloadType: 114, clockRate: 8000},
		{codec: "AMR-WB", payloadType: 104, clockRate: 16000},
	} {
		t.Run(test.codec, func(t *testing.T) {
			testReceiveOnlyBridge(t, test)
		})
	}
}

func testReceiveOnlyBridge(t *testing.T, config receiveOnlyTest) {
	browser, remoteTracks := newReceiveOnlyPCMUBrowserPeer(t)
	defer browser.Close()
	manager, err := NewMediaManager(receiveOnlyMediaOptions(config))
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	answer, err := manager.Create(context.Background(), "admin", completeLocalOffer(t, browser), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := browser.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: answer.SDP,
	}); err != nil {
		t.Fatal(err)
	}
	waitPeerConnected(t, browser)
	ims, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ims.Close()
	session := manager.Get(answer.MediaID)
	if session == nil || !session.receiveOnly {
		t.Fatal("browser media session is not receive-only")
	}
	recorder, err := newMixedRecorder(filepath.Join(t.TempDir(), "listen-only.wav"))
	if err != nil {
		t.Fatal(err)
	}
	session.SetRecorder(recorder)
	if err := session.Attach(receiveOnlyEndpointSDP(ims.LocalAddr().(*net.UDPAddr).Port, config)); err != nil {
		t.Fatal(err)
	}
	fixture := receiveOnlyFixture{ims: ims, session: session, remoteTracks: remoteTracks, config: config}
	assertSilentKeepalive(t, fixture)
	assertIMSDownlink(t, fixture)
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if recorder.dataBytes == 0 {
		t.Fatal("listen-only mixed recording contains no frames")
	}
}

func receiveOnlyMediaOptions(config receiveOnlyTest) MediaOptions {
	options := MediaOptions{UDPAddress: ":0"}
	if config.codec != "AMR" && config.codec != "AMR-WB" {
		return options
	}
	options.RealtimeCodecs = []string{config.codec}
	options.NewRealtimeCodec = func(codec, _ string) (RealtimeCodec, error) {
		if codec != config.codec {
			return nil, fmt.Errorf("unexpected codec %s", codec)
		}
		return &transportRealtimeCodec{sampleRate: config.clockRate}, nil
	}
	return options
}

func receiveOnlyEndpointSDP(port int, config receiveOnlyTest) string {
	if config.codec == "AMR" || config.codec == "AMR-WB" {
		return amrEndpointSDP(port, config.codec, config.payloadType, config.clockRate)
	}
	return g711EndpointSDP(port, config.codec, config.payloadType)
}

func newReceiveOnlyPCMUBrowserPeer(t *testing.T) (*webrtc.PeerConnection, <-chan *webrtc.TrackRemote) {
	t.Helper()
	engine := &webrtc.MediaEngine{}
	if err := engine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1,
		},
		PayloadType: 0,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatal(err)
	}
	peer, err := webrtc.NewAPI(webrtc.WithMediaEngine(engine)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peer.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		t.Fatal(err)
	}
	remoteTracks := make(chan *webrtc.TrackRemote, 1)
	peer.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) { remoteTracks <- remote })
	return peer, remoteTracks
}

func assertSilentKeepalive(t *testing.T, fixture receiveOnlyFixture) {
	t.Helper()
	for index := 1; index <= 3; index++ {
		packet := readUDPRTP(t, fixture.ims)
		if packet.PayloadType != fixture.config.payloadType || packet.SequenceNumber != uint16(index) {
			t.Fatalf("silent RTP header = %+v, want payload %d sequence %d",
				packet.Header, fixture.config.payloadType, index)
		}
		wantTimestamp := uint32(index * fixture.config.clockRate / audioFramesPerSecond)
		if packet.Timestamp != wantTimestamp {
			t.Fatalf("silent RTP timestamp = %d, want %d", packet.Timestamp, wantTimestamp)
		}
		assertSilentPayload(t, packet.Payload, fixture.config)
	}
}

func assertSilentPayload(t *testing.T, payload []byte, config receiveOnlyTest) {
	t.Helper()
	if config.codec == "AMR" || config.codec == "AMR-WB" {
		if string(payload) != "amr-frame" {
			t.Fatalf("silent %s RTP payload = %q", config.codec, payload)
		}
		return
	}
	wantSample := browserToIMSSample(0xff, config.codec)
	if len(payload) != browserSamplesPerFrame || payload[0] != wantSample {
		t.Fatalf("silent RTP payload = len %d sample %#x, want len %d sample %#x",
			len(payload), payload[0], browserSamplesPerFrame, wantSample)
	}
}

func assertIMSDownlink(t *testing.T, fixture receiveOnlyFixture) {
	t.Helper()
	endpoint, err := parseRTPEndpoint(fixture.session.PlainSDP())
	if err != nil {
		t.Fatal(err)
	}
	payload, wantSample := receiveOnlyDownlinkPayload(fixture.config)
	writeUDPRTP(t, fixture.ims, endpoint.Address, &rtp.Packet{Header: rtp.Header{
		Version: 2, PayloadType: fixture.config.payloadType, SequenceNumber: 100,
		Timestamp: 16000, SSRC: 0x2020,
	}, Payload: payload})
	packet := readRemoteRTP(t, waitRemoteTrack(t, fixture.remoteTracks))
	if packet.Payload[0] != wantSample {
		t.Fatalf("browser downlink sample = %#x, want %#x",
			packet.Payload[0], wantSample)
	}
}

func receiveOnlyDownlinkPayload(config receiveOnlyTest) ([]byte, byte) {
	if config.codec == "AMR" || config.codec == "AMR-WB" {
		return []byte("network-frame"), pcmToMuLaw(1000)
	}
	sample := imsEncodedSample(1200, config.codec)
	return repeatByte(sample, browserSamplesPerFrame), imsToBrowserSample(sample, config.codec)
}
