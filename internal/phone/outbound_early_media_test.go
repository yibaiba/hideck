package phone

import (
	"context"
	"fmt"
	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"net"
	"testing"
	"time"
)

func TestOutboundMediaAttachesBeforeAnswer(t *testing.T) {
	gateway := newFakeVoiceGateway()
	service := newPhoneTestService(t, gateway, newMemoryCallStore(), time.Second)
	relay, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	gateway.beginSnapshot.ClientSDP = fmt.Sprintf("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio %d RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n", relay.LocalAddr().(*net.UDPAddr).Port)
	addStubMedia(t, service, "early", "admin", "lease")
	call, err := service.StartCall(StartCallRequest{Owner: "admin", DeviceID: "dev-1", Callee: "10010", MediaID: "early", Lease: "lease"})
	if err != nil {
		t.Fatal(err)
	}
	if call.Status != StatusCalling || call.AnsweredAt != nil {
		t.Fatalf("early audio manufactured answer: %+v", call)
	}
	if err := relay.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := relay.ReadFromUDP(make([]byte, 2048)); err != nil {
		t.Fatalf("no RTP before answer: %v", err)
	}
	media := service.media.Get("early")
	media.mu.RLock()
	attached := media.attached
	recorder := media.recorder
	media.mu.RUnlock()
	if !attached || recorder != nil {
		t.Fatalf("attached=%v recorder=%v", attached, recorder)
	}
}

func TestBrowserReceivesCarrierAudioWhileCallIsRinging(t *testing.T) {
	gateway := newFakeVoiceGateway()
	service, err := NewService(ServiceOptions{Gateway: gateway, Store: newMemoryCallStore(), WebRTCUDPAddress: ":0", RecoveryGrace: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := service.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	browser, _, remoteTracks := newPCMUBrowserPeer(t)
	defer browser.Close()
	answer, err := service.media.Create(context.Background(), "admin", completeLocalOffer(t, browser), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := browser.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		t.Fatal(err)
	}
	waitPeerConnected(t, browser)
	modem, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer modem.Close()
	gateway.beginSnapshot.ClientSDP = g711EndpointSDP(modem.LocalAddr().(*net.UDPAddr).Port, "PCMU", 0)
	gateway.beginEvents = []voicehost.CallEvent{{Type: "CallRinging", DeviceID: "dev-1", CallID: gateway.beginSnapshot.CallID}}
	call, err := service.StartCall(StartCallRequest{Owner: "admin", DeviceID: "dev-1", Callee: "10010", MediaID: answer.MediaID, Lease: answer.Lease})
	if err != nil {
		t.Fatal(err)
	}
	if call.Status != StatusRinging || call.AnsweredAt != nil {
		t.Fatalf("state=%+v", call)
	}
	session := service.media.Get(answer.MediaID)
	endpoint, err := parseRTPEndpoint(session.PlainSDP())
	if err != nil {
		t.Fatal(err)
	}
	const sample = byte(0x82)
	for seq := uint16(100); seq < 103; seq++ {
		writeUDPRTP(t, modem, endpoint.Address, &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: seq, Timestamp: uint32(seq) * 160, SSRC: 0x2020}, Payload: repeatByte(sample, 160)})
	}
	packet := readRemoteRTP(t, waitRemoteTrack(t, remoteTracks))
	if len(packet.Payload) != 160 || packet.Payload[0] != sample {
		t.Fatalf("carrier audio changed: %+v", packet)
	}
}
