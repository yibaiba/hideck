package phone

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
	"github.com/pion/rtp"
)

func TestAutoAnswerTakeoverPreservesCallerRecording(t *testing.T) {
	relay, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	gateway := newFakeVoiceGateway()
	updates := make(chan string, 2)
	service, err := NewService(ServiceOptions{
		Gateway: &mediaUpdateGateway{fakeVoiceGateway: gateway, update: func(sdp string) error { updates <- sdp; return nil }},
		Store:   newMemoryCallStore(), RecordingDir: t.TempDir(), WebRTCUDPAddress: "127.0.0.1:0",
		PromptDecoder: promptDecoderStub{pcm: constantRecoveryFrame(6000), sampleRate: 8000},
		AutoAnswer: AutoAnswerConfig{AudioPath: "greeting.wav", Devices: map[string]AutoAnswerDeviceConfig{
			"dev-1": {Enabled: true, Delay: time.Millisecond},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	gateway.emitIncoming(voicehost.IncomingCall{
		DeviceID: "dev-1", CallID: "auto", Caller: "10010", ReceivedAt: time.Now(),
		OfferSDP: fmt.Sprintf("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio %d RTP/AVP 0\r\n", relay.LocalAddr().(*net.UDPAddr).Port),
	})
	old, recorder := waitAutoAnswerRecorder(t, service)
	gateway.emitEvent(voicehost.CallEvent{Type: "CallAnswered", DeviceID: "dev-1", CallID: "auto", Time: time.Now()})
	sendRecoveryCallerAudio(t, relay, recoveryAudioTarget{media: old, sample: 1000})
	verifyRecoveryRecording(t, recoveryRecordingOptions{service, relay, recorder, updates})
}

type recoveryRecordingOptions struct {
	service  *Service
	relay    *net.UDPConn
	recorder *mixedRecorder
	updates  <-chan string
}

func verifyRecoveryRecording(t *testing.T, options recoveryRecordingOptions) {
	service, relay, recorder := options.service, options.relay, options.recorder
	t.Helper()
	lease := ""
	for index, sample := range []int16{-2000, -3000} {
		media, err := service.media.CreateHeadless("admin")
		if err != nil {
			t.Fatal(err)
		}
		media.Lease = media.ID
		if _, _, err := service.RefreshMedia(RefreshRequest{Owner: "admin", CallID: "auto", MediaID: media.ID, Lease: lease, Takeover: index == 0}); err != nil {
			t.Fatal(err)
		}
		lease = media.Lease
		endpoint, err := parseRTPEndpoint(<-options.updates)
		if err != nil || endpoint.Address.String() != media.rtpConn.LocalAddr().String() {
			t.Fatalf("backend did not receive new relay endpoint: %v", err)
		}
		media.mu.RLock()
		reused := media.recorder == recorder
		media.mu.RUnlock()
		if !reused {
			t.Fatal("takeover replaced the auto-answer recording")
		}
		sendRecoveryCallerAudio(t, relay, recoveryAudioTarget{media: media, sample: sample})
		// Browser-side speech and the old prompt must remain excluded by policy.
		media.recordMixedFrame(mixToIMS, encodePCMU(constantRecoveryFrame(6000)))
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	assertRecoveryWAV(t, recorder.path)
}

func waitAutoAnswerRecorder(t *testing.T, service *Service) (*MediaSession, *mixedRecorder) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		service.mu.RLock()
		call := service.calls["auto"]
		mediaID, recorder := call.mediaID, call.mixedRecorder
		service.mu.RUnlock()
		if recorder != nil {
			return service.media.Get(mediaID), recorder
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("automatic answer did not start caller recording")
	return nil, nil
}

type recoveryAudioTarget struct {
	media  *MediaSession
	sample int16
}

func sendRecoveryCallerAudio(t *testing.T, relay *net.UDPConn, target recoveryAudioTarget) {
	t.Helper()
	for sequence := uint16(1); sequence <= 3; sequence++ {
		writeUDPRTP(t, relay, target.media.rtpConn.LocalAddr().(*net.UDPAddr), &rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: sequence, Timestamp: uint32(sequence) * browserSamplesPerFrame, SSRC: 7},
			Payload: encodePCMU(constantRecoveryFrame(target.sample)),
		})
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if target.media.Stats().PacketsFromIMS >= 3 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("caller audio was not decoded and forwarded by media session")
}

func constantRecoveryFrame(sample int16) []int16 {
	frame := make([]int16, browserSamplesPerFrame)
	for index := range frame {
		frame[index] = sample
	}
	return frame
}

func assertRecoveryWAV(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int16]bool{}
	for offset := mixedWAVHeaderBytes; offset+1 < len(data); offset += 2 {
		sample := int16(binary.LittleEndian.Uint16(data[offset:]))
		seen[sample] = true
		if sample > muLawToPCM(pcmToMuLaw(1000)) {
			t.Fatalf("prompt or browser audio contaminated recording: %d", sample)
		}
	}
	for _, sample := range []int16{1000, -2000, -3000} {
		if !seen[muLawToPCM(pcmToMuLaw(sample))] {
			t.Fatalf("recording lacks caller audio from takeover phase %d", sample)
		}
	}
}
