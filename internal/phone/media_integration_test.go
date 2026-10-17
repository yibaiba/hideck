package phone

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const mediaIntegrationTimeout = 3 * time.Second

func TestWebRTCBridgeSupportsG711AndPreservesRTPSequence(t *testing.T) {
	for _, test := range []struct {
		codec       string
		payloadType uint8
	}{{codec: "PCMU", payloadType: 0}, {codec: "PCMA", payloadType: 8}} {
		t.Run(test.codec, func(t *testing.T) {
			testWebRTCBridge(t, test.codec, test.payloadType)
		})
	}
}

func testWebRTCBridge(t *testing.T, imsCodec string, imsPayloadType uint8) {
	browser, browserTrack, remoteTracks := newPCMUBrowserPeer(t)
	defer browser.Close()
	offer := completeLocalOffer(t, browser)
	manager, err := NewMediaManager(MediaOptions{UDPAddress: ":0"})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	answer, err := manager.Create(context.Background(), "admin", offer, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := browser.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		t.Fatal(err)
	}
	waitPeerConnected(t, browser)
	ims, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ims.Close()
	session := manager.Get(answer.MediaID)
	if session == nil || !session.Matches("admin", answer.Lease) {
		t.Fatal("created media session or lease is unavailable")
	}
	if err := session.Attach(g711EndpointSDP(ims.LocalAddr().(*net.UDPAddr).Port, imsCodec, imsPayloadType)); err != nil {
		t.Fatal(err)
	}
	if endpoint, _, ok := session.endpoint(); !ok || endpoint.Codec != imsCodec {
		t.Fatalf("attached endpoint = %+v, attached=%t", endpoint, ok)
	}
	readUDPRTP(t, ims) // relay priming packet

	browserPayload := make([]byte, 160)
	for index := range browserPayload {
		browserPayload[index] = byte(index)
	}
	if err := browserTrack.WriteRTP(&rtp.Packet{Header: rtp.Header{
		Version: 2, PayloadType: 0, SequenceNumber: 30, Timestamp: 480, SSRC: 0x1010,
	}, Payload: browserPayload}); err != nil {
		t.Fatal(err)
	}
	toIMS := readUDPRTP(t, ims)
	if toIMS.PayloadType != imsPayloadType || toIMS.SequenceNumber != 30 || toIMS.Timestamp != 480 {
		t.Fatalf("browser->IMS RTP = %+v", toIMS.Header)
	}
	if got, want := toIMS.Payload[42], browserToIMSSample(browserPayload[42], imsCodec); got != want {
		t.Fatalf("browser->IMS payload = %#x, want %#x", got, want)
	}

	mediaAddress, err := parseRTPEndpoint(session.PlainSDP())
	if err != nil {
		t.Fatal(err)
	}
	imsSample := imsEncodedSample(1200, imsCodec)
	for _, sequence := range []uint16{100, 102, 101} {
		writeUDPRTP(t, ims, mediaAddress.Address, &rtp.Packet{Header: rtp.Header{
			Version: 2, PayloadType: imsPayloadType, SequenceNumber: sequence,
			Timestamp: uint32(sequence) * 160, SSRC: 0x2020,
		}, Payload: repeatByte(imsSample, 160)})
	}
	remoteTrack := waitRemoteTrack(t, remoteTracks)
	for _, wantSequence := range []uint16{100, 101, 102} {
		packet := readRemoteRTP(t, remoteTrack)
		if packet.PayloadType != 0 || packet.SequenceNumber != wantSequence {
			t.Fatalf("IMS->browser RTP = %+v, want sequence %d", packet.Header, wantSequence)
		}
		if got, want := packet.Payload[0], imsToBrowserSample(imsSample, imsCodec); got != want {
			t.Fatalf("IMS->browser payload = %#x, source=%#x want=%#x", got, imsSample, want)
		}
	}
	for _, sequence := range []uint16{103, 105} {
		writeUDPRTP(t, ims, mediaAddress.Address, &rtp.Packet{Header: rtp.Header{
			Version: 2, PayloadType: imsPayloadType, SequenceNumber: sequence,
			Timestamp: uint32(sequence) * 160, SSRC: 0x2020,
		}, Payload: repeatByte(imsSample, 160)})
	}
	for _, wantSequence := range []uint16{103, 105} {
		if packet := readRemoteRTP(t, remoteTrack); packet.SequenceNumber != wantSequence {
			t.Fatalf("RTP after loss has sequence %d, want %d", packet.SequenceNumber, wantSequence)
		}
	}
	stats := session.Stats()
	if stats.PacketsFromIMS != 5 || stats.PacketsToIMS == 0 || stats.PacketsLost != 1 {
		t.Fatalf("media stats = %+v", stats)
	}
}

func newPCMUBrowserPeer(t *testing.T) (*webrtc.PeerConnection, *webrtc.TrackLocalStaticRTP, <-chan *webrtc.TrackRemote) {
	t.Helper()
	engine := &webrtc.MediaEngine{}
	if err := engine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1},
		PayloadType:        0,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatal(err)
	}
	peer, err := webrtc.NewAPI(webrtc.WithMediaEngine(engine)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1,
	}, "microphone", "browser")
	if err != nil {
		t.Fatal(err)
	}
	sender, err := peer.AddTrack(track)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		buffer := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buffer); err != nil {
				return
			}
		}
	}()
	remoteTracks := make(chan *webrtc.TrackRemote, 1)
	peer.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) { remoteTracks <- remote })
	return peer, track, remoteTracks
}

func completeLocalOffer(t *testing.T, peer *webrtc.PeerConnection) string {
	t.Helper()
	gathering := webrtc.GatheringCompletePromise(peer)
	offer, err := peer.CreateOffer(nil)
	if err == nil {
		err = peer.SetLocalDescription(offer)
	}
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-gathering:
	case <-time.After(mediaIntegrationTimeout):
		t.Fatal("browser ICE gathering timed out")
	}
	return peer.LocalDescription().SDP
}

func waitPeerConnected(t *testing.T, peer *webrtc.PeerConnection) {
	t.Helper()
	deadline := time.Now().Add(mediaIntegrationTimeout)
	for time.Now().Before(deadline) {
		if peer.ConnectionState() == webrtc.PeerConnectionStateConnected {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("browser peer state = %s", peer.ConnectionState())
}

func readUDPRTP(t *testing.T, connection *net.UDPConn) *rtp.Packet {
	t.Helper()
	_ = connection.SetReadDeadline(time.Now().Add(mediaIntegrationTimeout))
	buffer := make([]byte, 2048)
	read, _, err := connection.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}
	packet := &rtp.Packet{}
	if err := packet.Unmarshal(buffer[:read]); err != nil {
		t.Fatal(err)
	}
	return packet
}

func writeUDPRTP(t *testing.T, connection *net.UDPConn, target *net.UDPAddr, packet *rtp.Packet) {
	t.Helper()
	raw, err := packet.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.WriteToUDP(raw, target); err != nil {
		t.Fatal(err)
	}
}

func waitRemoteTrack(t *testing.T, tracks <-chan *webrtc.TrackRemote) *webrtc.TrackRemote {
	t.Helper()
	select {
	case track := <-tracks:
		return track
	case <-time.After(mediaIntegrationTimeout):
		t.Fatal("browser did not receive remote audio track")
		return nil
	}
}

func readRemoteRTP(t *testing.T, track *webrtc.TrackRemote) *rtp.Packet {
	t.Helper()
	result := make(chan *rtp.Packet, 1)
	errors := make(chan error, 1)
	go func() {
		packet, _, err := track.ReadRTP()
		if err != nil {
			errors <- err
			return
		}
		result <- packet
	}()
	select {
	case packet := <-result:
		return packet
	case err := <-errors:
		t.Fatal(err)
	case <-time.After(mediaIntegrationTimeout):
		t.Fatal("browser RTP read timed out")
	}
	return nil
}

func g711EndpointSDP(port int, codec string, payloadType uint8) string {
	return fmt.Sprintf(
		"v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio %d RTP/AVP %d\r\na=rtpmap:%d %s/8000\r\n",
		port, payloadType, payloadType, codec,
	)
}

func browserToIMSSample(sample byte, codec string) byte {
	if codec == "PCMA" {
		return pcmToALaw(muLawToPCM(sample))
	}
	return sample
}

func imsEncodedSample(sample int16, codec string) byte {
	if codec == "PCMA" {
		return pcmToALaw(sample)
	}
	return pcmToMuLaw(sample)
}

func imsToBrowserSample(sample byte, codec string) byte {
	if codec == "PCMA" {
		return pcmToMuLaw(aLawToPCM(sample))
	}
	return sample
}

func repeatByte(value byte, count int) []byte {
	result := make([]byte, count)
	for index := range result {
		result[index] = value
	}
	return result
}
