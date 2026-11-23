package phone

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
)

func TestAnswerUsesReplacementMediaEndpoint(t *testing.T) {
	gateway := newFakeVoiceGateway()
	service := newPhoneTestService(t, gateway, newMemoryCallStore(), time.Second)
	service.recordingDir = t.TempDir()
	replacement, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	port := replacement.LocalAddr().(*net.UDPAddr).Port
	gateway.answerSDP = fmt.Sprintf("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio %d RTP/AVP 0\r\n", port)
	gateway.emitIncoming(voicehost.IncomingCall{DeviceID: "dev-1", CallID: "incoming", Caller: "10010", OfferSDP: testPlainSDP})
	addStubMedia(t, service, "media-1", "admin", "lease-1")
	if _, err := service.Answer(context.Background(), ControlRequest{Owner: "admin", CallID: "incoming", MediaID: "media-1", Lease: "lease-1"}); err != nil {
		t.Fatal(err)
	}
	// Attach primes the exact relay endpoint used after retrying audio startup.
	if err := replacement.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := replacement.ReadFromUDP(make([]byte, 2048)); err != nil {
		t.Fatal("replacement endpoint received no RTP", err)
	}
}

// A call waiting behind another one has no SDP: the AT backend only knows a
// line index. Answer used to attach that empty string and fail with "SDP has
// no usable RTP endpoint", never reaching the modem. The backend opens the
// endpoint while answering, so the phone must ask first and attach afterwards.
func TestAnswerWithoutAnIncomingOfferIsNegotiatedByTheBackend(t *testing.T) {
	gateway := newFakeVoiceGateway()
	service := newPhoneTestService(t, gateway, newMemoryCallStore(), time.Second)
	service.recordingDir = t.TempDir()
	replacement, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	port := replacement.LocalAddr().(*net.UDPAddr).Port
	gateway.answerSDP = fmt.Sprintf("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio %d RTP/AVP 0\r\n", port)
	gateway.emitIncoming(voicehost.IncomingCall{DeviceID: "dev-1", CallID: "modemvoice-waiting", Caller: "10086"})
	addStubMedia(t, service, "media-1", "admin", "lease-1")
	if _, err := service.Answer(context.Background(), ControlRequest{Owner: "admin", CallID: "modemvoice-waiting", MediaID: "media-1", Lease: "lease-1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-gateway.answerCalls:
		if _, err := parseRTPEndpoint(request.SDP); err != nil {
			t.Fatalf("answer offered an unusable endpoint: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("answer never reached the gateway")
	}
	if err := replacement.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := replacement.ReadFromUDP(make([]byte, 2048)); err != nil {
		t.Fatal("endpoint opened by the answer received no RTP", err)
	}
}

func TestAnswerRejectsIMSWithoutAnOffer(t *testing.T) {
	gateway := newFakeVoiceGateway()
	service := newPhoneTestService(t, gateway, newMemoryCallStore(), time.Second)
	gateway.emitIncoming(voicehost.IncomingCall{DeviceID: "dev-1", CallID: "ims-no-offer"})
	addStubMedia(t, service, "media-1", "admin", "lease-1")
	_, err := service.Answer(context.Background(), ControlRequest{Owner: "admin", CallID: "ims-no-offer", MediaID: "media-1", Lease: "lease-1"})
	if err == nil {
		t.Fatal("missing IMS offer accepted")
	}
	select {
	case <-gateway.answerCalls:
		t.Fatal("IMS call answered without media")
	default:
	}
	select {
	case reject := <-gateway.rejectCalls:
		if reject.StatusCode != 488 {
			t.Fatalf("reject=%+v", reject)
		}
	default:
		t.Fatal("invalid offer was not rejected")
	}
}

func TestAnswerEndsATCallWhenBackendReturnsNoEndpoint(t *testing.T) {
	gateway := newFakeVoiceGateway()
	service := newPhoneTestService(t, gateway, newMemoryCallStore(), time.Second)
	gateway.emitIncoming(voicehost.IncomingCall{DeviceID: "dev-1", CallID: "modemvoice-no-offer"})
	addStubMedia(t, service, "media-1", "admin", "lease-1")
	_, err := service.Answer(context.Background(), ControlRequest{Owner: "admin", CallID: "modemvoice-no-offer", MediaID: "media-1", Lease: "lease-1"})
	if err == nil {
		t.Fatal("missing AT answer endpoint accepted")
	}
	select {
	case callID := <-gateway.hangupCalls:
		if callID != "modemvoice-no-offer" {
			t.Fatalf("hung up %q", callID)
		}
	default:
		t.Fatal("answered call left without media or ownership")
	}
}
