package phone

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func TestWebRTCBridgeSchedulesAMRAndAMRWBFrames(t *testing.T) {
	for _, test := range []struct {
		codec       string
		payloadType uint8
		clockRate   int
	}{
		{codec: "AMR", payloadType: 114, clockRate: 8000},
		{codec: "AMR-WB", payloadType: 104, clockRate: 16000},
	} {
		t.Run(test.codec, func(t *testing.T) {
			testWebRTCAMRBridge(t, test.codec, test.payloadType, test.clockRate)
		})
	}
}

func testWebRTCAMRBridge(t *testing.T, imsCodec string, payloadType uint8, clockRate int) {
	browser, browserTrack, remoteTracks := newPCMUBrowserPeer(t)
	defer browser.Close()
	manager, err := NewMediaManager(MediaOptions{
		UDPAddress: ":0", RealtimeCodecs: []string{imsCodec},
		NewRealtimeCodec: func(codec, fmtp string) (RealtimeCodec, error) {
			if codec != imsCodec || fmtp != "octet-align=1" {
				return nil, fmt.Errorf("unexpected codec request %s %s", codec, fmtp)
			}
			return &transportRealtimeCodec{sampleRate: clockRate}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	answer, err := manager.Create(context.Background(), "admin", completeLocalOffer(t, browser), "")
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
	if err := session.Attach(amrEndpointSDP(ims.LocalAddr().(*net.UDPAddr).Port, imsCodec, payloadType, clockRate)); err != nil {
		t.Fatal(err)
	}
	readUDPRTP(t, ims)

	browserPayload := repeatByte(pcmToMuLaw(1000), 160)
	if err := browserTrack.WriteRTP(&rtp.Packet{Header: rtp.Header{
		Version: 2, PayloadType: 0, SequenceNumber: 30, Timestamp: 480, SSRC: 0x3030,
	}, Payload: browserPayload}); err != nil {
		t.Fatal(err)
	}
	toIMS := readUDPRTP(t, ims)
	if toIMS.PayloadType != payloadType || toIMS.SequenceNumber != 30 ||
		toIMS.Timestamp != scaleTimestamp(480, browserClockRate, clockRate) || string(toIMS.Payload) != "amr-frame" {
		t.Fatalf("browser->IMS packet = %+v payload=%q", toIMS.Header, toIMS.Payload)
	}

	mediaAddress, err := parseRTPEndpoint(session.PlainSDP())
	if err != nil {
		t.Fatal(err)
	}
	writeUDPRTP(t, ims, mediaAddress.Address, &rtp.Packet{Header: rtp.Header{
		Version: 2, PayloadType: payloadType, SequenceNumber: 70,
		Timestamp: uint32(clockRate / 5), SSRC: 0x4040,
	}, Payload: []byte("network-frame")})
	fromIMS := readRemoteRTP(t, waitRemoteTrack(t, remoteTracks))
	if fromIMS.PayloadType != 0 || fromIMS.SequenceNumber != 70 || fromIMS.Timestamp != browserClockRate/5 {
		t.Fatalf("IMS->browser packet = %+v", fromIMS.Header)
	}
	if len(fromIMS.Payload) != 160 || fromIMS.Payload[0] != pcmToMuLaw(1000) {
		t.Fatalf("IMS->browser payload length=%d first=%#x", len(fromIMS.Payload), fromIMS.Payload[0])
	}
}

func amrEndpointSDP(port int, codec string, payloadType uint8, clockRate int) string {
	return fmt.Sprintf(
		"v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio %d RTP/AVP %d\r\na=rtpmap:%d %s/%d\r\na=fmtp:%d octet-align=1\r\n",
		port, payloadType, payloadType, codec, clockRate, payloadType,
	)
}

type transportRealtimeCodec struct{ sampleRate int }

func (codec *transportRealtimeCodec) SampleRate() int { return codec.sampleRate }

func (codec *transportRealtimeCodec) Decode([]byte) ([]int16, error) {
	pcm := make([]int16, codec.sampleRate/50)
	for index := range pcm {
		pcm[index] = 1000
	}
	return pcm, nil
}

func (*transportRealtimeCodec) Encode([]int16) ([]byte, error) {
	return []byte("amr-frame"), nil
}

func (*transportRealtimeCodec) Close() error { return nil }
