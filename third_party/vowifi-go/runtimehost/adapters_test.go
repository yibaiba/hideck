package runtimehost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iniwex5/vowifi-go/internal/vowifi/imscore"
	"github.com/iniwex5/vowifi-go/internal/vowifi/runtimecore"
	"github.com/iniwex5/vowifi-go/internal/vowifi/smsdelivery"
	"github.com/iniwex5/vowifi-go/internal/vowifi/voice"
	"github.com/iniwex5/vowifi-go/runtimehost/eventhost"
	"github.com/iniwex5/vowifi-go/runtimehost/identity"
	"github.com/iniwex5/vowifi-go/runtimehost/messaging"
	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
)

// stubAKA is a deterministic AKA provider.
type stubAKA struct{}

func (stubAKA) CalculateAKA(rand16, autn16 []byte) (imscore.AKAResult, error) {
	return imscore.AKAResult{
		RES: []byte{0x33, 0x33, 0x33, 0x33, 0x33, 0x33, 0x33, 0x33, 0x33, 0x33, 0x33, 0x33, 0x33, 0x33, 0x33, 0x33},
		CK:  []byte{0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11},
		IK:  []byte{0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22},
	}, nil
}

// newTestService builds an imscore service for adapter tests.
func newTestService(t *testing.T) *imscore.Service {
	t.Helper()
	return newTestServiceWithRegistrar(t, startTestRegistrar(t))
}

func newTestServiceWithRegistrar(t *testing.T, registrar *net.UDPConn) *imscore.Service {
	t.Helper()
	plainSIP := false
	cfg := &imscore.IMSConfig{
		DeviceID:        "dev-1",
		IMSI:            "310260123456789",
		IMPI:            "310260123456789@ims.example.com",
		Domain:          "ims.example.com",
		SMSC:            "+123",
		LocalIP:         net.IPv4(127, 0, 0, 1),
		Registrar:       registrar.LocalAddr().String(),
		AKAProvider:     stubAKA{},
		EnableIPSec3GPP: &plainSIP,
	}
	svc, err := imscore.New(cfg)
	if err != nil {
		t.Fatalf("imscore.New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := svc.Register(ctx); err != nil {
		t.Fatalf("imscore.Register: %v", err)
	}
	return svc
}

func startTestRegistrar(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buffer := make([]byte, 64*1024)
		for {
			n, remote, readErr := conn.ReadFromUDP(buffer)
			if readErr != nil {
				return
			}
			request := string(buffer[:n])
			body := ""
			extraHeaders := ""
			if strings.HasPrefix(request, "REGISTER ") {
				extraHeaders = "P-Associated-URI: <sip:+15551234567@ims.example.com>\r\n" +
					"Contact: " + testSIPHeader(request, "Contact") + "\r\n"
			}
			if strings.HasPrefix(request, "INVITE ") {
				body = `<?xml version="1.0"?><ussd-data><language>en</language><ussd-string>Balance: 10</ussd-string><UnstructuredSS-Notify/></ussd-data>`
				extraHeaders = "To: <sip:ussi@ims.example.com>;tag=test-remote\r\n" +
					"Contact: <sip:ussi@ims.example.com>\r\n" +
					"Content-Type: application/vnd.3gpp.ussd+xml\r\n"
			}
			response := fmt.Sprintf("SIP/2.0 200 OK\r\nVia: %s\r\nCall-ID: %s\r\nCSeq: %s\r\n%sContent-Length: %d\r\n\r\n%s",
				testSIPHeader(request, "Via"), testSIPHeader(request, "Call-ID"), testSIPHeader(request, "CSeq"), extraHeaders, len(body), body)
			_, _ = conn.WriteToUDP([]byte(response), remote)
		}
	}()
	return conn
}

func testSIPHeader(message, name string) string {
	for _, line := range strings.Split(message, "\r\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func TestServiceAdapterStatus(t *testing.T) {
	svc := newTestService(t)
	adapter := newServiceAdapter(svc)
	st := adapter.StatusCurrent()
	if st.State.RegStatus != 1 {
		t.Errorf("reg status = %d, want 1 (registered)", st.State.RegStatus)
	}
	if !st.State.IMSReady {
		t.Error("IMSReady should be true")
	}
	if !st.State.SMSReady {
		t.Errorf("SMSReady should be true: %s", st.State.SMSReadyReason)
	}
	if !st.State.SMSMOReady {
		t.Errorf("SMSMOReady should be true: %s", st.State.SMSReadyReason)
	}
	if st.State.DeviceID != "dev-1" {
		t.Errorf("device = %q", st.State.DeviceID)
	}
}

func TestServiceAdapterSMS(t *testing.T) {
	svc := newTestService(t)
	adapter := newServiceAdapter(svc)
	out, err := adapter.SendSMSWithResult(context.Background(), "+8613800000000", "hi")
	if err != nil {
		t.Fatalf("SendSMSWithResult: %v", err)
	}
	if out.Ref == "" {
		t.Error("SMS ref should not be empty")
	}
}

func TestServiceAdapterUSSD(t *testing.T) {
	svc := newTestService(t)
	adapter := newServiceAdapter(svc)
	res, err := adapter.SendUSSD(context.Background(), "*100#")
	if err != nil {
		t.Fatalf("SendUSSD: %v", err)
	}
	if res.Status != 0 || res.Text != "Balance: 10" {
		t.Errorf("ussd result = %+v", res)
	}
}

func TestServiceAdapterNoService(t *testing.T) {
	adapter := newServiceAdapter(nil)
	if _, err := adapter.SendSMSWithResult(context.Background(), "1", "x"); !errors.Is(err, errNoService) {
		t.Errorf("err = %v, want errNoService", err)
	}
}

func TestAdaptSMSSendOutcomePreservesIdentity(t *testing.T) {
	out := adaptSMSSendOutcome(imscore.SendOutcome{
		MessageID: "sms-1", PartsTotal: 1, DeliveryState: "failed",
		SIPCode: 503, RecommendCSFallback: true,
	})
	if out.MessageID != "sms-1" || out.Ref != "sms-1" || out.PartsTotal != 1 {
		t.Fatalf("delivery identity = %+v", out)
	}
	if out.DeliveryState != "failed" || out.Err != nil || out.SIPCode != 503 || !out.RecommendCSFallback {
		t.Fatalf("delivery failure = %+v", out)
	}
}

func TestAdaptSMSReadinessPreservesMOSendWithoutMTReceiver(t *testing.T) {
	readiness := adaptSMSReadiness(imscore.SMSReadiness{
		Registered: true, ProfileReady: true, TransportReady: true,
		SMSCPresent: true, MOReady: true, Reason: "IMS SMS receiver is not ready",
	})
	instance := &Instance{}
	instance.setState(State{IMSReady: true})
	instance.updateSMSReadiness(readiness)

	state := instance.State()
	if !state.SMSMOReady {
		t.Fatalf("MO SMS readiness was lost across runtime adapters: %+v", state)
	}
	if state.SMSReady {
		t.Fatalf("MT SMS readiness was incorrectly enabled: %+v", state)
	}
}

func TestVoiceAgentAttachAndStopCleanup(t *testing.T) {
	svc := newTestService(t)
	gateway := voicehost.NewGateway()
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	inst := &Instance{}
	inst.setService(newServiceAdapter(svc))
	req := StartRequest{DeviceID: "dev-1", VoiceGateway: gateway}
	if err := attachVoiceAgent(req, inst, &imscoreLifecycleAdapter{svc: svc}); err != nil {
		t.Fatalf("attachVoiceAgent: %v", err)
	}
	if gateway.GetAgent("dev-1") == nil || gateway.DeviceStatusCurrent("dev-1")["ready"] != true {
		t.Fatalf("voice status = %+v", gateway.DeviceStatus("dev-1"))
	}
	if err := inst.Stop(context.Background()); err != nil {
		t.Fatalf("Instance.Stop: %v", err)
	}
	if gateway.GetAgent("dev-1") != nil {
		t.Fatal("voice agent remained attached after runtime stop")
	}
}

func TestVoiceGatewaySimulateCallUsesProductionAdapterAndRTP(t *testing.T) {
	mediaConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer mediaConn.Close()
	requests := make(chan string, 16)
	registrar := startVoiceAdapterRegistrar(t, mediaConn.LocalAddr().(*net.UDPAddr).Port, requests)
	svc := newTestServiceWithRegistrar(t, registrar)
	gateway := voicehost.NewGateway()
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer gateway.Stop()
	inst := &Instance{}
	inst.setService(newServiceAdapter(svc))
	if err := attachVoiceAgent(StartRequest{DeviceID: "dev-1", VoiceGateway: gateway}, inst, &imscoreLifecycleAdapter{svc: svc}); err != nil {
		t.Fatal(err)
	}
	defer inst.Stop(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	captureDirectory := t.TempDir()
	mediaControlDone := make(chan error, 1)
	result, err := gateway.SimulateCall(ctx, "dev-1", voicehost.SimulateCallRequest{
		Callee: "+8613800000000", HoldSeconds: 1,
		OnConnected: func() {
			mediaControlDone <- startProductionMediaControls(gateway, "dev-1", captureDirectory)
		},
	})
	if err != nil || !result.Success {
		t.Fatalf("SimulateCall result=%+v err=%v", result, err)
	}
	if err := <-mediaControlDone; err != nil {
		t.Fatalf("media controls: %v", err)
	}
	assertProductionVoiceMedia(t, mediaConn)
	assertProductionVoicePCAP(t, captureDirectory)
	assertProductionVoiceRequests(t, requests)
	agent := gateway.GetAgent("dev-1").(*voice.Agent)
	if agent.IsBusy() || agent.SnapshotCurrent().ActiveCall != nil {
		t.Fatalf("call remained active after timed BYE: %+v", agent.SnapshotCurrent())
	}
}

func startProductionMediaControls(gateway *voicehost.Gateway, deviceID, directory string) error {
	agent, ok := gateway.GetAgent(deviceID).(*voice.Agent)
	if !ok || agent == nil || agent.SnapshotCurrent().ActiveCall == nil {
		return errors.New("production voice call is not registered")
	}
	callID := agent.SnapshotCurrent().ActiveCall.CallID
	if err := gateway.StartPCAP(deviceID, directory); err != nil {
		return err
	}
	return gateway.SendDTMF(deviceID, callID, "2")
}

func TestVoiceGatewayReceivesAndRejectsProductionIncomingCall(t *testing.T) {
	requests := make(chan string, 32)
	registeredClient := make(chan *net.UDPAddr, 1)
	registrar := startVoiceAdapterRegistrar(t, 40000, requests, registeredClient)
	svc := newTestServiceWithRegistrar(t, registrar)
	clientAddress := <-registeredClient
	gateway := voicehost.NewGateway()
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer gateway.Stop()
	incoming := make(chan voicehost.IncomingCall, 1)
	notified := make(chan productionIncomingNotification, 1)
	dispatched := make(chan eventhost.Event, 4)
	gateway.SetNotifier(productionCallNotifier{notifications: notified})
	gateway.SetEventDispatcher(eventhostDispatcherFunc(func(_ context.Context, event eventhost.Event) {
		dispatched <- event
	}))
	gateway.SetIncomingCallHandler(func(call voicehost.IncomingCall) { incoming <- call })
	inst := &Instance{}
	inst.setService(newServiceAdapter(svc))
	if err := attachVoiceAgent(StartRequest{DeviceID: "dev-1", VoiceGateway: gateway}, inst, &imscoreLifecycleAdapter{svc: svc}); err != nil {
		t.Fatal(err)
	}
	defer inst.Stop(context.Background())

	callID := "incoming-runtimehost-1"
	invite := productionIncomingInvite(registrar.LocalAddr().String(), callID)
	if _, err := registrar.WriteToUDP([]byte(invite), clientAddress); err != nil {
		t.Fatal(err)
	}
	call := awaitProductionIncomingCall(t, incoming)
	if call.CallID != callID || call.DeviceID != "dev-1" {
		t.Fatalf("incoming call = %+v", call)
	}
	awaitProductionCallNotification(t, notified, "dev-1", "+15557654321", "+15551234567")
	awaitProductionVoiceEvent(t, dispatched, "IncomingCall", "dev-1")
	calls, err := gateway.IncomingCalls("dev-1")
	if err != nil || len(calls) != 1 || calls[0].CallID != callID {
		t.Fatalf("registry calls=%+v error=%v", calls, err)
	}
	if err := gateway.RejectIncomingCall(voicehost.RejectRequest{DeviceID: "dev-1", CallID: callID}); err != nil {
		t.Fatal(err)
	}
	awaitProductionSIPStatus(t, requests, 486)
}

type eventhostDispatcherFunc func(context.Context, eventhost.Event)

func (dispatch eventhostDispatcherFunc) Dispatch(ctx context.Context, event eventhost.Event) {
	dispatch(ctx, event)
}

type productionIncomingNotification struct {
	deviceID string
	caller   string
	callee   string
}

type productionCallNotifier struct {
	notifications chan<- productionIncomingNotification
}

func (notifier productionCallNotifier) NotifyIncomingCall(deviceID, caller, callee string) {
	notifier.notifications <- productionIncomingNotification{
		deviceID: deviceID,
		caller:   caller,
		callee:   callee,
	}
}

func awaitProductionCallNotification(
	t *testing.T,
	notifications <-chan productionIncomingNotification,
	deviceID, caller, callee string,
) {
	t.Helper()
	select {
	case notification := <-notifications:
		if notification.deviceID != deviceID || notification.caller != caller || notification.callee != callee {
			t.Fatalf("incoming notification = %+v", notification)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for incoming call notification")
	}
}

func productionIncomingInvite(registrarAddress, callID string) string {
	body := "v=0\r\no=- 3 3 IN IP4 127.0.0.1\r\ns=ims\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 40000 RTP/AVP 0 101\r\n"
	return fmt.Sprintf("INVITE sip:+15551234567@ims.example.com SIP/2.0\r\nVia: SIP/2.0/UDP %s;branch=z9hG4bK-incoming\r\nMax-Forwards: 70\r\nFrom: <sip:+15557654321@ims.example.com>;tag=remote-incoming\r\nTo: <sip:+15551234567@ims.example.com>\r\nCall-ID: %s\r\nCSeq: 1 INVITE\r\nContact: <sip:+15557654321@%s>\r\nContent-Type: application/sdp\r\nContent-Length: %d\r\n\r\n%s", registrarAddress, callID, registrarAddress, len(body), body)
}

func awaitProductionIncomingCall(t *testing.T, calls <-chan voicehost.IncomingCall) voicehost.IncomingCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for production incoming call")
		return voicehost.IncomingCall{}
	}
}

func awaitProductionSIPStatus(t *testing.T, requests <-chan string, status int) {
	t.Helper()
	want := fmt.Sprintf("SIP/2.0 %d ", status)
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case message := <-requests:
			if strings.HasPrefix(message, want) {
				return
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for %d response", status)
		}
	}
}

func awaitProductionVoiceEvent(
	t *testing.T,
	events <-chan eventhost.Event,
	eventType, deviceID string,
) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if event != nil && event.Type() == eventType && event.DeviceID() == deviceID {
				return
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for %s/%s event", eventType, deviceID)
		}
	}
}

func startVoiceAdapterRegistrar(
	t *testing.T,
	mediaPort int,
	requests chan<- string,
	registeredClient ...chan<- *net.UDPAddr,
) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buffer := make([]byte, 64*1024)
		for {
			n, remote, readErr := conn.ReadFromUDP(buffer)
			if readErr != nil {
				return
			}
			request := string(buffer[:n])
			if strings.HasPrefix(request, "REGISTER ") && len(registeredClient) != 0 {
				address := *remote
				select {
				case registeredClient[0] <- &address:
				default:
				}
			}
			select {
			case requests <- request:
			default:
			}
			if strings.HasPrefix(request, "ACK ") || strings.HasPrefix(request, "SIP/2.0 ") {
				continue
			}
			body, extra := "", ""
			if strings.HasPrefix(request, "REGISTER ") {
				extra = "P-Associated-URI: <sip:+15551234567@ims.example.com>\r\n" +
					"Contact: " + testSIPHeader(request, "Contact") + "\r\n"
			}
			if strings.HasPrefix(request, "INVITE ") {
				body = fmt.Sprintf("v=0\r\no=- 2 2 IN IP4 127.0.0.1\r\ns=ims\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio %d RTP/AVP 0 101\r\na=rtpmap:101 telephone-event/8000\r\na=fmtp:101 0-16\r\n", mediaPort)
				extra = "To: <sip:callee@ims.example.com>;tag=voice-remote\r\nContact: <sip:callee@ims.example.com>\r\nContent-Type: application/sdp\r\n"
			}
			response := fmt.Sprintf("SIP/2.0 200 OK\r\nVia: %s\r\nCall-ID: %s\r\nCSeq: %s\r\n%sContent-Length: %d\r\n\r\n%s",
				testSIPHeader(request, "Via"), testSIPHeader(request, "Call-ID"), testSIPHeader(request, "CSeq"), extra, len(body), body)
			_, _ = conn.WriteToUDP([]byte(response), remote)
		}
	}()
	return conn
}

func assertProductionVoiceMedia(t *testing.T, mediaConn *net.UDPConn) {
	t.Helper()
	if err := mediaConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	packet := make([]byte, 256)
	seenAudio, seenDTMF := false, false
	for !seenAudio || !seenDTMF {
		n, _, err := mediaConn.ReadFromUDP(packet)
		if err != nil {
			t.Fatalf("read production adapter RTP: %v (audio=%v DTMF=%v)", err, seenAudio, seenDTMF)
		}
		if n == 172 && packet[1]&0x7f == 0 {
			seenAudio = true
		}
		if n >= 16 && packet[1]&0x7f == 101 && packet[12] == 2 {
			seenDTMF = true
		}
	}
}

func assertProductionVoicePCAP(t *testing.T, directory string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(directory, "rtp_*.pcap"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("PCAP paths=%v error=%v", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(data) <= 24 {
		t.Fatalf("PCAP length = %d, want packet records", len(data))
	}
}

func assertProductionVoiceRequests(t *testing.T, requests <-chan string) {
	t.Helper()
	seen := map[string]bool{}
	for {
		select {
		case request := <-requests:
			method := strings.Fields(request)[0]
			seen[method] = true
			if method == "INVITE" && (strings.Contains(request, "m=audio 0 ") || !strings.Contains(request, "RTP/AVP 104 110 102 108 96 101") || strings.Contains(request, "a=rtpmap:0 PCMU") || strings.Contains(request, "EVS")) {
				t.Fatalf("INVITE did not advertise the production media codec set: %q", request)
			}
		default:
			if !seen["INVITE"] || !seen["ACK"] || !seen["BYE"] {
				t.Fatalf("voice requests = %+v, want INVITE ACK BYE", seen)
			}
			return
		}
	}
}

// memDeliveryStore is an in-memory delivery store for adapter tests.
type memDeliveryStore struct {
	status        *messaging.DeliveryStatus
	sipCode       int
	inboundStored bool
	degraded      bool
}

type fragmentOnlyDeliveryStore struct {
	messaging.DeliveryStore
	fragments messaging.InboundFragmentStore
}

type subscriptionRejectionDeliveryStore struct {
	messaging.DeliveryStore
	status    int
	expiresAt time.Time
}

func (store *subscriptionRejectionDeliveryStore) LoadIMSSubscriptionRejection(
	_, _ string,
	_ time.Time,
) (int, time.Time, error) {
	return store.status, store.expiresAt, nil
}

func (store *subscriptionRejectionDeliveryStore) SaveIMSSubscriptionRejection(
	_, _ string,
	status int,
	expiresAt time.Time,
) error {
	store.status, store.expiresAt = status, expiresAt
	return nil
}

func (store *subscriptionRejectionDeliveryStore) DeleteIMSSubscriptionRejections(string) error {
	store.status, store.expiresAt = 0, time.Time{}
	return nil
}

func (store fragmentOnlyDeliveryStore) LoadInboundFragments(
	owner messaging.InboundFragmentOwner,
) ([]messaging.StoredInboundFragment, error) {
	return store.fragments.LoadInboundFragments(owner)
}

func (store fragmentOnlyDeliveryStore) SaveInboundFragment(
	scope messaging.InboundFragmentScope,
	fragment messaging.InboundFragment,
) (messaging.InboundFragmentSaveResult, error) {
	return store.fragments.SaveInboundFragment(scope, fragment)
}

func (store fragmentOnlyDeliveryStore) DeleteInboundFragments(
	scope messaging.InboundFragmentScope,
) error {
	return store.fragments.DeleteInboundFragments(scope)
}

func (store fragmentOnlyDeliveryStore) MarkInboundFragmentAcked(
	scope messaging.InboundFragmentScope,
	sequence int,
	at time.Time,
) error {
	return store.fragments.MarkInboundFragmentAcked(scope, sequence, at)
}

func (m *memDeliveryStore) CreateSMSDelivery(
	messageID, imsi, deviceID, peer, content string,
	partsTotal int,
	_ time.Time,
) error {
	return m.CreateSMSDeliveryForIdentity(messaging.DeliveryRecord{
		MessageID: messageID, IMSI: imsi, DeviceID: deviceID,
		Peer: peer, Content: content, PartsTotal: partsTotal,
	})
}

func (m *memDeliveryStore) CreateSMSDeliveryForIdentity(record messaging.DeliveryRecord) error {
	m.status = &messaging.DeliveryStatus{
		MessageID: record.MessageID, ICCID: record.ICCID, IMSI: record.IMSI,
		DeviceID: record.DeviceID, Peer: record.Peer, Content: record.Content,
		PartsTotal: record.PartsTotal, State: "accepted",
	}
	return nil
}
func (m *memDeliveryStore) UpsertSMSDeliveryPart(messageID string, partNo int, callID string, rpMR int, state string, sentAt time.Time) error {
	return nil
}
func (m *memDeliveryStore) MarkSMSDeliveryPartSIPResult(messageID string, partNo, sipCode int, state, errText string, at time.Time) error {
	m.sipCode = sipCode
	return nil
}
func (m *memDeliveryStore) MarkSMSDeliveryPartReport(inReplyTo, callID, deviceID string, rpMR int, state string, sipCode int, rpCause int, errText string, at time.Time) (messaging.DeliveryPartMatch, error) {
	return messaging.DeliveryPartMatch{MessageID: inReplyTo, PartNo: rpMR, State: state, Matched: true}, nil
}
func (m *memDeliveryStore) RecomputeSMSDelivery(messageID string, at time.Time) error { return nil }
func (m *memDeliveryStore) UpdateSMSDeliveryState(messageID, state, lastError string, acks int, at time.Time) error {
	return nil
}
func (m *memDeliveryStore) GetSMSDeliveryStatus(messageID string) (*messaging.DeliveryStatus, error) {
	if m.status == nil {
		return nil, messaging.ErrDeliveryNotFound
	}
	return m.status, nil
}
func (m *memDeliveryStore) LoadInboundFragments(messaging.InboundFragmentOwner) ([]messaging.StoredInboundFragment, error) {
	return nil, nil
}
func (m *memDeliveryStore) SaveInboundFragment(
	messaging.InboundFragmentScope,
	messaging.InboundFragment,
) (messaging.InboundFragmentSaveResult, error) {
	m.inboundStored = true
	return messaging.InboundFragmentSaveResult{Inserted: true}, nil
}
func (m *memDeliveryStore) DeleteInboundFragments(messaging.InboundFragmentScope) error { return nil }
func (m *memDeliveryStore) MarkInboundFragmentAcked(
	messaging.InboundFragmentScope,
	int,
	time.Time,
) error {
	return nil
}
func (m *memDeliveryStore) MarkInboundFragmentsDegraded(
	messaging.InboundFragmentScope,
	time.Time,
) error {
	m.degraded = true
	return nil
}

func TestDeliveryStoreAdapter(t *testing.T) {
	store := &memDeliveryStore{}
	adapter := newDeliveryStoreAdapter(store)
	identityAdapter, ok := adapter.(imscore.SMSDeliveryIdentityStore)
	if !ok {
		t.Fatal("adapter did not preserve delivery identity capability")
	}
	if err := identityAdapter.CreateSMSDeliveryForIdentity(messaging.DeliveryRecord{
		MessageID: "msg-1", ICCID: "8901000000000000001", IMSI: "310260123456789",
		DeviceID: "dev-1", Peer: "+8613800000000", Content: "hi", PartsTotal: 1, At: time.Now(),
	}); err != nil {
		t.Fatalf("CreateSMSDelivery: %v", err)
	}
	st, err := adapter.GetSMSDeliveryStatus("msg-1")
	if err != nil {
		t.Fatalf("GetSMSDeliveryStatus: %v", err)
	}
	if st.MessageID != "msg-1" || st.State != "accepted" {
		t.Errorf("status = %+v", st)
	}
	sipResults, ok := adapter.(imscore.SMSDeliverySIPResultStore)
	if !ok {
		t.Fatal("adapter did not preserve SIP result capability")
	}
	if err := sipResults.MarkSMSDeliveryPartSIPResult("msg-1", 1, 202, "pending", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if store.sipCode != 202 {
		t.Fatalf("persisted SIP code = %d", store.sipCode)
	}
	fragments, ok := adapter.(imscore.SMSInboundFragmentStore)
	if !ok {
		t.Fatal("adapter did not preserve inbound fragment capability")
	}
	if _, err := fragments.SaveInboundFragment(
		messaging.InboundFragmentScope{}, messaging.InboundFragment{},
	); err != nil || !store.inboundStored {
		t.Fatalf("SaveInboundFragment err=%v stored=%v", err, store.inboundStored)
	}
	lifecycle, ok := adapter.(imscore.SMSInboundFragmentLifecycleStore)
	if !ok {
		t.Fatal("adapter did not preserve inbound fragment lifecycle capability")
	}
	if err := lifecycle.MarkInboundFragmentsDegraded(messaging.InboundFragmentScope{}, time.Now()); err != nil || !store.degraded {
		t.Fatalf("MarkInboundFragmentsDegraded err=%v degraded=%v", err, store.degraded)
	}
}

func TestRuntimeCoreDeliveryStorePreservesOptionalSIPResults(t *testing.T) {
	store := &memDeliveryStore{}
	adapter := runtimeCoreDeliveryStore(store)
	sipResults, ok := adapter.(smsdelivery.SIPResultStore)
	if !ok {
		t.Fatal("runtimehost runtimecore adapter lost SIP result capability")
	}
	if err := sipResults.MarkSMSDeliveryPartSIPResult(
		"msg-1", 1, 202, "pending", "", time.Now(),
	); err != nil {
		t.Fatalf("MarkSMSDeliveryPartSIPResult: %v", err)
	}
	if store.sipCode != 202 {
		t.Fatalf("persisted SIP code = %d", store.sipCode)
	}
	fragments, ok := adapter.(smsdelivery.InboundFragmentStore)
	if !ok {
		t.Fatal("runtimehost runtimecore adapter lost inbound fragment capability")
	}
	if _, err := fragments.SaveInboundFragment(
		smsdelivery.InboundFragmentScope{}, smsdelivery.InboundFragment{},
	); err != nil || !store.inboundStored {
		t.Fatalf("SaveInboundFragment err=%v stored=%v", err, store.inboundStored)
	}
	lifecycle, ok := adapter.(smsdelivery.InboundFragmentLifecycleStore)
	if !ok {
		t.Fatal("runtimehost runtimecore adapter lost fragment lifecycle capability")
	}
	if err := lifecycle.MarkInboundFragmentsDegraded(smsdelivery.InboundFragmentScope{}, time.Now()); err != nil || !store.degraded {
		t.Fatalf("MarkInboundFragmentsDegraded err=%v degraded=%v", err, store.degraded)
	}
}

func TestRuntimeCoreAdapterPreservesSubscriptionRejectionStore(t *testing.T) {
	store := &subscriptionRejectionDeliveryStore{DeliveryStore: &memDeliveryStore{}}
	persistence := runtimeCoreSubscriptionRejectionStore(store)
	if persistence == nil {
		t.Fatal("subscription rejection store was dropped")
	}
	expiresAt := time.Now().Add(time.Hour)
	if err := persistence.SaveIMSSubscriptionRejection(
		"identity", "reg", 489, expiresAt,
	); err != nil {
		t.Fatal(err)
	}
	status, loadedExpiry, err := persistence.LoadIMSSubscriptionRejection(
		"identity", "reg", time.Now(),
	)
	if err != nil || status != 489 || !loadedExpiry.Equal(expiresAt) {
		t.Fatalf("status=%d expiry=%s err=%v", status, loadedExpiry, err)
	}
}

type mwiPolicyDeliveryStore struct {
	messaging.DeliveryStore
	rejected int
}

func (store *mwiPolicyDeliveryStore) LoadMWISubscriptionPolicy(string, string) (string, int, error) {
	return "on", store.rejected, nil
}

func (store *mwiPolicyDeliveryStore) SaveMWISubscriptionRejection(_, _ string, status int) error {
	store.rejected = status
	return nil
}

func TestCoreRequestCarriesMWIPolicyStore(t *testing.T) {
	request := restoredRuntimeRequest()
	request.DeliveryStore = &mwiPolicyDeliveryStore{DeliveryStore: &memDeliveryStore{}}
	policy := request.coreRequest().MWIPolicyStore
	if policy == nil {
		t.Fatal("core request dropped the MWI policy store")
	}
	if err := policy.SaveMWISubscriptionRejection("iccid", "identity", 405); err != nil {
		t.Fatal(err)
	}
	if mode, rejected, err := policy.LoadMWISubscriptionPolicy("iccid", "identity"); err != nil || mode != "on" || rejected != 405 {
		t.Fatalf("mode=%q rejected=%d err=%v", mode, rejected, err)
	}
	request.DeliveryStore = &memDeliveryStore{}
	if request.coreRequest().MWIPolicyStore != nil {
		t.Fatal("core request invented an MWI policy store")
	}
}

func TestCustomTunnelStartWiresMWIPolicy(t *testing.T) {
	cfg := &imscore.IMSConfig{}
	applyDeliveryStores(cfg, &mwiPolicyDeliveryStore{DeliveryStore: &memDeliveryStore{}})
	if cfg.MWIPolicyStore == nil || cfg.SubscriptionRegistrations == nil || cfg.DeliveryStore == nil {
		t.Fatalf("custom tunnel IMS config = %+v", cfg)
	}
	template, _, err := imsRegisterConfigForPrepared(&identity.PreparedSession{
		Profile: identity.Profile{MCC: "262", MNC: "02"},
	})
	if err != nil || !template.DisableMWISubscription {
		t.Fatalf("Vodafone DE custom tunnel template = %+v err=%v", template, err)
	}
}

func TestDeliveryStoreAdaptersDoNotInventFragmentLifecycleCapability(t *testing.T) {
	underlying := &memDeliveryStore{}
	store := fragmentOnlyDeliveryStore{DeliveryStore: underlying, fragments: underlying}
	adapters := []any{newDeliveryStoreAdapter(store), runtimeCoreDeliveryStore(store)}
	for _, adapter := range adapters {
		if _, ok := adapter.(smsdelivery.InboundFragmentStore); !ok {
			t.Fatalf("adapter %T lost fragment capability", adapter)
		}
		if _, ok := adapter.(smsdelivery.InboundFragmentLifecycleStore); ok {
			t.Fatalf("adapter %T invented lifecycle capability", adapter)
		}
	}
}

func TestRuntimeCoreDeliveryStorePreservesMatchedReport(t *testing.T) {
	store := &memDeliveryStore{}
	match, err := runtimeCoreDeliveryStore(store).MarkSMSDeliveryPartReport(
		"msg-1", "call-1", "dev-1", 17, "acked", 200, 0, "", time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !match.Matched || match.MessageID != "msg-1" || match.PartNo != 17 || match.State != "acked" {
		t.Fatalf("report match = %+v", match)
	}
}

func TestDeliveryStatusConversion(t *testing.T) {
	createdAt := time.Date(2026, time.August, 10, 1, 58, 25, 0, time.UTC)
	updatedAt := createdAt.Add(300 * time.Millisecond)
	reportAt := updatedAt
	internal := &imscore.DeliveryStatus{
		MessageID:  "m1",
		IMSI:       "310260123456789",
		DeviceID:   "dev-1",
		Peer:       "+8613800000000",
		Content:    "hi",
		PartsTotal: 1,
		Acks:       1,
		State:      "delivered",
		CreatedAt:  createdAt,
		UpdatedAt:  updatedAt,
		Parts: []imscore.DeliveryPartStatus{{
			PartNo: 1, CallID: "c1", InReplyTo: "reply-c1", RPMR: 23,
			State: "delivered", SIPCode: 200, RPCause: 0, RPCauseText: "accepted",
			ErrorText: "", SentAt: createdAt, ReportAt: &reportAt,
			CreatedAt: createdAt, UpdatedAt: updatedAt,
		}},
	}
	ext := deliveryStatusFromInternal(internal)
	if ext.MessageID != "m1" || ext.State != "delivered" || ext.CreatedAt != createdAt || ext.UpdatedAt != updatedAt {
		t.Errorf("converted = %+v", ext)
	}
	if len(ext.Parts) != 1 || ext.Parts[0].SIPCode != 200 || ext.Parts[0].RPMR != 23 ||
		ext.Parts[0].InReplyTo != "reply-c1" || ext.Parts[0].ReportAt == nil || *ext.Parts[0].ReportAt != reportAt {
		t.Errorf("parts = %+v", ext.Parts)
	}
	back := deliveryStatusToInternal(ext)
	if back.MessageID != "m1" || back.CreatedAt != createdAt || len(back.Parts) != 1 ||
		back.Parts[0].RPMR != 23 || back.Parts[0].UpdatedAt != updatedAt {
		t.Errorf("round-trip = %+v", back)
	}
}

func TestStartInstanceAsync(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	runner := func(ctx context.Context, _ runtimecore.RuntimeStartRequest) (StartResult, error) {
		close(started)
		<-ctx.Done()
		return StartResult{TraceID: "trace-1"}, ctx.Err()
	}
	instance, err := startInstanceAsync(ctx, runtimecore.RuntimeStartRequest{
		DeviceID: "dev-1", TraceID: "trace-1",
	}, runtimeLaunchOptions{runner: runner, delay: defaultReaderReconnectDelay})
	if err != nil || instance == nil {
		t.Fatalf("startInstanceAsync instance=%v err=%v", instance, err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runtime runner was not started")
	}
	cancel()
}
