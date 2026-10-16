package imscore

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/iniwex5/vowifi-go/internal/vowifi/ipsec3gpp"
)

type captureIPSecNetwork struct {
	*SystemIMSNetwork
	policy    ipsec3gpp.Policy
	installed bool
}

// This test network models the kernel/IPsec boundary; crypto is tested in
// ipsec3gpp and the packet bridge rather than on these plaintext loopback sockets.
func (n *captureIPSecNetwork) ListenProtectedUDP(addr *net.UDPAddr) (net.PacketConn, error) {
	return n.ListenPacket("udp", addr)
}

func (n *captureIPSecNetwork) InstallIPSec3GPP(policy ipsec3gpp.Policy) error {
	validated, err := ipsec3gpp.NewPolicy(policy)
	if err != nil {
		return err
	}
	n.policy = validated
	n.installed = true
	return nil
}

func TestRegisterNegotiatesAndInstallsIPSec3GPP(t *testing.T) {
	network := &captureIPSecNetwork{SystemIMSNetwork: NewSystemIMSNetwork(net.IPv4(10, 0, 0, 2))}
	svc := newSecurityAgreementTestService(t, network)
	var initialClient string
	serverHeader := ""
	svc.transport.SetSendFn(func(request string) error {
		if strings.Contains(request, "CSeq: 2 REGISTER") {
			initialClient = sipHeaderValue(request, "Security-Client")
			assertInitialSecurityHeaders(t, request, initialClient)
			serverHeader = serverOfferForClient(t, initialClient)
			svc.transport.DeliverResponse(akaChallengeResponse(request, serverHeader))
			return nil
		}
		if !network.installed {
			t.Fatal("authenticated REGISTER sent before IPsec policy installation")
		}
		assertAuthenticatedSecurityHeaders(t, request, initialClient, serverHeader)
		if got := svc.currentRegistrationRemote().Port; got != 51001 {
			t.Fatalf("protected registrar port = %d, want 51001", got)
		}
		svc.transport.DeliverResponse(registerResponseForRequest(request, 200, nil))
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := svc.Register(ctx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	assertInstalledPolicy(t, network.policy)
	if got := svc.GetSecurityVerify(); got != serverHeader {
		t.Fatalf("Security-Verify = %q, want %q", got, serverHeader)
	}
}

func TestRegisterSwitchesFromInitialUDPToProtectedTCP(t *testing.T) {
	udpServer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udpServer.Close()
	tcpServer, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer tcpServer.Close()
	result := make(chan error, 1)
	go serveUDPChallengeThenTCPSuccess(udpServer, tcpServer, result)

	network := &captureIPSecNetwork{SystemIMSNetwork: NewSystemIMSNetwork(net.IPv4(127, 0, 0, 1))}
	svc, err := New(&IMSConfig{
		DeviceID: "dev-sec-tcp", IMEI: "860349055895064", IMSI: "234102356143376",
		IMPI: "234102356143376@ims.example", IMPU: "sip:234102356143376@ims.example",
		Domain: "ims.example", LocalIP: net.IPv4(127, 0, 0, 1), Transport: "auto",
		Registrar: udpServer.LocalAddr().String(), IMSNetwork: network,
		AKAProvider: stubAKAProvider{}, EnableIPSec3GPP: enabledBoolPointer(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.StopCurrent()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := svc.Register(ctx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if !network.installed || !svc.IsRegistered() {
		t.Fatal("protected TCP registration did not complete")
	}
}

func TestRegisterPreservesIPSecReservationsAcrossInitialTransportFallback(t *testing.T) {
	initialTCP, initialUDP := listenRegisterTransports(t)
	defer initialTCP.Close()
	defer initialUDP.Close()
	protectedTCP, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer protectedTCP.Close()

	udpSeen := make(chan string, 1)
	go observeInitialUDPRegister(initialUDP, udpSeen)
	serverResult := make(chan error, 1)
	releaseServer := make(chan struct{})
	go serveFallbackTCPChallenge(initialTCP, protectedTCP, releaseServer, serverResult)

	network := &captureIPSecNetwork{SystemIMSNetwork: NewSystemIMSNetwork(net.IPv4(127, 0, 0, 1))}
	svc, err := New(&IMSConfig{
		DeviceID: "dev-sec-fallback", IMEI: "860349055895064", IMSI: "234159612558315",
		IMPI: "234159612558315@ims.example", IMPU: "sip:234159612558315@ims.example",
		Domain: "ims.example", LocalIP: net.IPv4(127, 0, 0, 1), Transport: "auto",
		Registrar: initialUDP.LocalAddr().String(), IMSNetwork: network,
		AKAProvider: stubAKAProvider{}, EnableIPSec3GPP: enabledBoolPointer(),
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.transport.timers = sipTransactionTimers{
		t1: 5 * time.Millisecond, t2: 10 * time.Millisecond, bf: 40 * time.Millisecond,
		d: 5 * time.Millisecond, k: 5 * time.Millisecond, m: 40 * time.Millisecond,
	}
	t.Cleanup(func() {
		svc.StopCurrent()
		close(releaseServer)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := svc.Register(ctx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// The sec-agree REGISTER is over the RFC 3261 UDP limit, so auto moves it
	// to TCP before sending; the switch must keep the IPsec reservations.
	select {
	case request := <-udpSeen:
		t.Fatalf("oversized initial REGISTER went over UDP: %q", sipHeaderValue(request, "Via"))
	default:
	}
	select {
	case err := <-serverResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("protected TCP registration did not complete")
	}
	if !network.installed || !svc.IsRegistered() {
		t.Fatal("UDP timeout fallback did not complete protected TCP registration")
	}
}

func TestRegisterKeepsInitialTCPUntilProtectedRegisterCompletes(t *testing.T) {
	initial, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer initial.Close()
	protected, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer protected.Close()
	result := make(chan error, 1)
	go serveMakeBeforeBreakRegistration(initial, protected, result)

	network := &captureIPSecNetwork{SystemIMSNetwork: NewSystemIMSNetwork(net.IPv4(127, 0, 0, 1))}
	svc, err := New(&IMSConfig{
		DeviceID: "dev-sec-mbb", IMEI: "860349055895064", IMSI: "234102356143376",
		IMPI: "234102356143376@ims.example", IMPU: "sip:234102356143376@ims.example",
		Domain: "ims.example", LocalIP: net.IPv4(127, 0, 0, 1), Transport: "tcp",
		Registrar: initial.Addr().String(), IMSNetwork: network,
		AKAProvider: stubAKAProvider{}, EnableIPSec3GPP: enabledBoolPointer(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.StopCurrent()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := svc.Register(ctx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func serveMakeBeforeBreakRegistration(initial, protected *net.TCPListener, result chan<- error) {
	initialConn, err := initial.AcceptTCP()
	if err != nil {
		result <- err
		return
	}
	defer initialConn.Close()
	initialRequest, err := readSIPStreamMessage(bufio.NewReader(initialConn))
	if err != nil {
		result <- err
		return
	}
	client, err := parseSecurityMechanism(splitSecurityMechanisms(sipHeaderValue(initialRequest, "Security-Client"))[0])
	if err != nil {
		result <- err
		return
	}
	serverHeader := fmt.Sprintf("ipsec-3gpp;q=0.98;alg=hmac-sha-1-96;mod=trans;ealg=aes-cbc;spi-c=858993459;spi-s=1145324612;port-c=6059;port-s=%d", tcpPort(protected.Addr()))
	challenge := strings.TrimPrefix(strings.TrimSpace(digestChallengeHeaderNoQOP()), "WWW-Authenticate: ")
	headers := "WWW-Authenticate: " + challenge + "\r\nSecurity-Server: " + serverHeader + "\r\n"
	if _, err = initialConn.Write([]byte(registerWireResponse(initialRequest, 401, headers))); err != nil {
		result <- err
		return
	}
	protectedConn, err := protected.AcceptTCP()
	if err != nil {
		result <- err
		return
	}
	defer protectedConn.Close()
	authenticated, err := readSIPStreamMessage(bufio.NewReader(protectedConn))
	if err != nil {
		result <- err
		return
	}
	if err := assertTCPConnectionOpen(initialConn); err != nil {
		result <- err
		return
	}
	if tcpPort(protectedConn.RemoteAddr()) != int(client.PortC) {
		result <- fmt.Errorf("protected source port = %d, want %d", tcpPort(protectedConn.RemoteAddr()), client.PortC)
		return
	}
	if _, err = protectedConn.Write([]byte(registerWireResponse(authenticated, 200, ""))); err != nil {
		result <- err
		return
	}
	result <- waitForTCPConnectionClose(initialConn)
}

func assertTCPConnectionOpen(conn *net.TCPConn) error {
	if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		return err
	}
	var probe [1]byte
	_, err := conn.Read(probe[:])
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		return conn.SetReadDeadline(time.Time{})
	}
	if err == nil {
		return errors.New("initial REGISTER connection received unexpected data")
	}
	return fmt.Errorf("initial REGISTER connection closed before protected response: %w", err)
}

func waitForTCPConnectionClose(conn *net.TCPConn) error {
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	var probe [1]byte
	_, err := conn.Read(probe[:])
	if errors.Is(err, io.EOF) {
		return nil
	}
	return fmt.Errorf("initial REGISTER connection remained open: %w", err)
}

func serveUDPChallengeThenTCPSuccess(udpServer *net.UDPConn, tcpServer *net.TCPListener, result chan<- error) {
	buffer := make([]byte, 64*1024)
	n, remote, err := udpServer.ReadFromUDP(buffer)
	if err != nil {
		result <- err
		return
	}
	initial := string(buffer[:n])
	initialSentBy := sipViaSentBy(sipHeaderValue(initial, "Via"))
	if initialSentBy == "" || !strings.HasPrefix(sipHeaderValue(initial, "Via"), "SIP/2.0/UDP ") {
		result <- errors.New("IPsec auto registration did not start on UDP")
		return
	}
	client, err := parseSecurityMechanism(splitSecurityMechanisms(sipHeaderValue(initial, "Security-Client"))[0])
	if err != nil {
		result <- err
		return
	}
	serverHeader := fmt.Sprintf("ipsec-3gpp;q=0.98;alg=hmac-sha-1-96;mod=trans;ealg=aes-cbc;spi-c=858993459;spi-s=1145324612;port-c=6059;port-s=%d", tcpPort(tcpServer.Addr()))
	challenge := strings.TrimPrefix(strings.TrimSpace(digestChallengeHeaderNoQOP()), "WWW-Authenticate: ")
	headers := "WWW-Authenticate: " + challenge + "\r\nSecurity-Server: " + serverHeader + "\r\n"
	if _, err := udpServer.WriteToUDP([]byte(registerWireResponse(initial, 401, headers)), remote); err != nil {
		result <- err
		return
	}
	conn, err := tcpServer.AcceptTCP()
	if err != nil {
		result <- err
		return
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		result <- err
		return
	}
	if tcpPort(conn.RemoteAddr()) != int(client.PortC) {
		result <- fmt.Errorf("protected TCP source port = %d, want %d", tcpPort(conn.RemoteAddr()), client.PortC)
		return
	}
	reader := bufio.NewReader(conn)
	authenticated, err := readSIPStreamMessage(reader)
	if err != nil {
		result <- err
		return
	}
	if via := sipHeaderValue(authenticated, "Via"); sipViaSentBy(via) != initialSentBy {
		result <- fmt.Errorf("protected Via sent-by = %q, want initial sent-by %q", sipViaSentBy(via), initialSentBy)
		return
	}
	if sipHeaderValue(authenticated, "Security-Verify") != serverHeader || strings.Contains(sipHeaderValue(authenticated, "Authorization"), "qop=") {
		result <- errors.New("authenticated REGISTER did not preserve recovered security or Digest shape")
		return
	}
	registerHeaders := "P-Associated-URI: <sip:+447840844894@o2.co.uk>,<tel:+447840844894>\r\n" +
		"Service-Route: <sip:pcscf.example;lr>\r\n"
	if _, err = conn.Write([]byte(registerWireResponse(authenticated, 200, registerHeaders))); err != nil {
		result <- err
		return
	}
	subscribe, err := readRegistrationSubscriptionAfterMWI(conn, reader)
	if err != nil {
		result <- err
		return
	}
	if err := assertRecoveredRegistrationSubscription(subscribe, serverHeader, client); err != nil {
		result <- err
		return
	}
	_, err = conn.Write([]byte(registerWireResponse(subscribe, 200, "")))
	result <- err
}

// Registration and MWI subscriptions are independent; either may arrive first.
func readRegistrationSubscriptionAfterMWI(conn net.Conn, reader *bufio.Reader) (string, error) {
	request, err := readSIPStreamMessage(reader)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(request, "SUBSCRIBE ") && sipHeaderValue(request, "Event") == "message-summary" {
		if _, err := conn.Write([]byte(registerWireResponse(request, 200, ""))); err != nil {
			return "", err
		}
		return readSIPStreamMessage(reader)
	}
	return request, nil
}

func observeInitialUDPRegister(conn *net.UDPConn, seen chan<- string) {
	buffer := make([]byte, 64*1024)
	n, _, err := conn.ReadFromUDP(buffer)
	if err == nil {
		seen <- string(buffer[:n])
	}
}

func serveFallbackTCPChallenge(
	initial, protected *net.TCPListener,
	release <-chan struct{},
	result chan<- error,
) {
	initialConn, err := initial.AcceptTCP()
	if err != nil {
		result <- err
		return
	}
	defer initialConn.Close()
	_ = initialConn.SetDeadline(time.Now().Add(3 * time.Second))
	request, err := readSIPStreamMessage(bufio.NewReader(initialConn))
	if err != nil {
		result <- err
		return
	}
	mechanisms := splitSecurityMechanisms(sipHeaderValue(request, "Security-Client"))
	if len(mechanisms) == 0 {
		result <- errors.New("fallback REGISTER omitted Security-Client")
		return
	}
	client, err := parseSecurityMechanism(mechanisms[0])
	if err != nil {
		result <- err
		return
	}
	serverHeader := fmt.Sprintf("ipsec-3gpp;q=0.98;alg=hmac-sha-1-96;mod=trans;ealg=aes-cbc;spi-c=858993459;spi-s=1145324612;port-c=6059;port-s=%d", tcpPort(protected.Addr()))
	challenge := strings.TrimPrefix(strings.TrimSpace(digestChallengeHeaderNoQOP()), "WWW-Authenticate: ")
	headers := "WWW-Authenticate: " + challenge + "\r\nSecurity-Server: " + serverHeader + "\r\n"
	if _, err = initialConn.Write([]byte(registerWireResponse(request, 401, headers))); err != nil {
		result <- err
		return
	}
	serveFallbackProtectedRegister(protected, client, serverHeader, release, result)
}

func serveFallbackProtectedRegister(
	listener *net.TCPListener,
	client securityMechanism,
	serverHeader string,
	release <-chan struct{},
	result chan<- error,
) {
	conn, err := listener.AcceptTCP()
	if err != nil {
		result <- err
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	request, err := readSIPStreamMessage(bufio.NewReader(conn))
	if err == nil && tcpPort(conn.RemoteAddr()) != int(client.PortC) {
		err = fmt.Errorf("protected TCP source port = %d, want %d", tcpPort(conn.RemoteAddr()), client.PortC)
	}
	if err == nil && (sipHeaderValue(request, "Security-Verify") != serverHeader || sipHeaderValue(request, "Authorization") == "") {
		err = errors.New("protected REGISTER omitted security agreement or AKA authorization")
	}
	if err == nil {
		headers := "P-Associated-URI: <sip:+447840844894@ims.example>\r\nService-Route: <sip:pcscf.example;lr>\r\n"
		_, err = conn.Write([]byte(registerWireResponse(request, 200, headers)))
	}
	result <- err
	if err == nil {
		<-release
	}
}

func sipViaSentBy(via string) string {
	fields := strings.Fields(via)
	if len(fields) < 2 {
		return ""
	}
	sentBy, _, _ := strings.Cut(fields[1], ";")
	return sentBy
}

func assertRecoveredRegistrationSubscription(request, securityVerify string, client securityMechanism) error {
	if !strings.HasPrefix(request, "SUBSCRIBE sip:+447840844894@o2.co.uk SIP/2.0") {
		return fmt.Errorf("unexpected SUBSCRIBE request line: %q", strings.SplitN(request, "\r\n", 2)[0])
	}
	checks := map[string]string{
		"Event":                "reg",
		"Accept":               "application/reginfo+xml",
		"P-Preferred-Identity": "<sip:+447840844894@o2.co.uk>",
		"Security-Verify":      securityVerify,
		"Require":              "sec-agree",
		"Proxy-Require":        "sec-agree",
	}
	for name, want := range checks {
		if got := sipHeaderValue(request, name); got != want {
			return fmt.Errorf("SUBSCRIBE %s = %q, want %q", name, got, want)
		}
	}
	route := sipHeaderValue(request, "Route")
	if !strings.Contains(route, "<sip:pcscf.example;lr>") || !strings.Contains(route, ";transport=tcp;lr>") {
		return fmt.Errorf("SUBSCRIBE Route = %q, want P-CSCF then Service-Route", route)
	}
	if !strings.Contains(sipHeaderValue(request, "Via"), fmt.Sprintf(":%d;", client.PortC)) ||
		!strings.Contains(sipHeaderValue(request, "Contact"),
			fmt.Sprintf(":%d;ob;transport=tcp>", client.PortS)) {
		return errors.New("SUBSCRIBE did not advertise the negotiated protected ports")
	}
	return nil
}

func digestChallengeHeaderNoQOP() string {
	nonce := base64Std(append(bytes.Repeat([]byte{0x11}, 16), bytes.Repeat([]byte{0x22}, 16)...))
	return fmt.Sprintf("WWW-Authenticate: Digest realm=\"ims.example\", nonce=\"%s\", algorithm=AKAv1-MD5\r\n", nonce)
}

func newSecurityAgreementTestService(t *testing.T, network IMSNetwork) *Service {
	t.Helper()
	svc, err := New(&IMSConfig{
		DeviceID: "dev-sec", IMEI: "356938035643809", IMSI: "310260123456789", IMPI: "310260123456789@ims.example",
		IMPU: "sip:310260123456789@ims.example", Domain: "ims.example",
		LocalIP: net.IPv4(10, 0, 0, 2), LocalPort: 41000, Transport: "udp", Expires: time.Hour,
		AKAProvider: stubAKAProvider{}, IMSNetwork: network, EnableIPSec3GPP: enabledBoolPointer(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	svc.protectedServerPort = 41001
	svc.registrationRemote = &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 5060}
	t.Cleanup(svc.StopCurrent)
	return svc
}

func assertInitialSecurityHeaders(t *testing.T, request, securityClient string) {
	t.Helper()
	if len(splitSecurityMechanisms(securityClient)) != 6 || !strings.Contains(securityClient, "alg=hmac-md5-96") ||
		!strings.Contains(securityClient, "ealg=des-ede3-cbc") || !strings.Contains(securityClient, "ealg=null") {
		t.Fatalf("initial Security-Client is invalid: %q", securityClient)
	}
	if strings.Contains(securityClient, "prot=") || strings.Contains(securityClient, "mod=") {
		t.Fatalf("initial Security-Client has non-original parameters: %q", securityClient)
	}
	if sipHeaderValue(request, "Require") != "sec-agree" || sipHeaderValue(request, "Proxy-Require") != "sec-agree" {
		t.Fatalf("initial REGISTER did not require sec-agree: %q", request)
	}
	if sipHeaderValue(request, "Security-Verify") != "" {
		t.Fatal("initial REGISTER unexpectedly contained Security-Verify")
	}
	if !strings.Contains(sipHeaderValue(request, "Via"), "10.0.0.2:41000;") ||
		!strings.Contains(sipHeaderValue(request, "Contact"), "@10.0.0.2:41001") {
		t.Fatal("initial REGISTER did not advertise the recovered Via and Contact ports")
	}
	if !strings.Contains(sipHeaderValue(request, "Authorization"), `nonce=""`) {
		t.Fatal("initial REGISTER omitted the empty IMS AKA authorization")
	}
}

func serverOfferForClient(t *testing.T, clientHeader string) string {
	t.Helper()
	client, err := parseSecurityMechanism(splitSecurityMechanisms(clientHeader)[0])
	if err != nil {
		t.Fatalf("parse generated Security-Client: %v", err)
	}
	spiC, spiS := uint32(0x33333333), uint32(0x44444444)
	if client.SPIC == spiC || client.SPIS == spiC {
		spiC++
	}
	if client.SPIC == spiS || client.SPIS == spiS || spiC == spiS {
		spiS++
	}
	return fmt.Sprintf("ipsec-3gpp;alg=hmac-sha-1-96;ealg=aes-cbc;prot=esp;mod=trans;spi-c=%d;spi-s=%d;port-c=51000;port-s=51001", spiC, spiS)
}

func akaChallengeResponse(request, securityServer string) *sipResponse {
	randBytes := bytes.Repeat([]byte{0x11}, 16)
	autnBytes := bytes.Repeat([]byte{0x22}, 16)
	nonce := base64Std(append(append([]byte{}, randBytes...), autnBytes...))
	headers := map[string]string{
		"WWW-Authenticate": `Digest realm="ims.example", nonce="` + nonce + `", algorithm=AKAv1-MD5, qop="auth"`,
	}
	if securityServer != "" {
		headers["Security-Server"] = securityServer
	}
	return registerResponseForRequest(request, 401, headers)
}

func assertAuthenticatedSecurityHeaders(t *testing.T, request, client, server string) {
	t.Helper()
	if sipHeaderValue(request, "Security-Client") != client {
		t.Fatal("authenticated REGISTER changed Security-Client")
	}
	if sipHeaderValue(request, "Security-Verify") != server {
		t.Fatal("authenticated REGISTER did not mirror Security-Server")
	}
	if sipHeaderValue(request, "Authorization") == "" {
		t.Fatal("authenticated REGISTER omitted Digest-AKA authorization")
	}
	via := sipHeaderValue(request, "Via")
	contact := sipHeaderValue(request, "Contact")
	if !strings.HasPrefix(via, "SIP/2.0/TCP 10.0.0.2:41000;") || !strings.Contains(via, ";alias") ||
		!strings.Contains(contact, "@10.0.0.2:41001") {
		t.Fatalf("authenticated REGISTER did not use the recovered protected TCP shape: Via=%q Contact=%q", via, contact)
	}
}

func assertInstalledPolicy(t *testing.T, policy ipsec3gpp.Policy) {
	t.Helper()
	if !policy.LocalIP.Equal(net.IPv4(10, 0, 0, 2)) || !policy.RemoteIP.Equal(net.IPv4(10, 0, 0, 1)) {
		t.Fatalf("policy endpoints = %s -> %s", policy.LocalIP, policy.RemoteIP)
	}
	if policy.LocalPortC != 41000 || policy.LocalPortS != 41001 ||
		policy.RemotePortC != 51000 || policy.RemotePortS != 51001 {
		t.Fatalf("policy ports = %+v", policy)
	}
	if policy.FlowC.OutboundSPI != 0x44444444 || policy.FlowS.OutboundSPI != 0x33333333 ||
		policy.FlowC.InboundSPI == 0 || policy.FlowS.InboundSPI == 0 ||
		policy.FlowC.InboundSPI == policy.FlowS.InboundSPI {
		t.Fatalf("policy flow SPIs = C:%+v S:%+v", policy.FlowC, policy.FlowS)
	}
	if policy.FlowC.EncAlg != ipsec3gpp.EncryptionAES ||
		!bytes.Equal(policy.FlowC.CK, bytes.Repeat([]byte{0x11}, 16)) ||
		!bytes.Equal(policy.FlowC.IK, bytes.Repeat([]byte{0x22}, 16)) {
		t.Fatal("policy did not retain negotiated algorithm and AKA keys")
	}
}

func TestRegisterRejectsMissingSecurityServer(t *testing.T) {
	network := &captureIPSecNetwork{SystemIMSNetwork: NewSystemIMSNetwork(net.IPv4(10, 0, 0, 2))}
	svc := newSecurityAgreementTestService(t, network)
	svc.transport.SetSendFn(func(request string) error {
		svc.transport.DeliverResponse(akaChallengeResponse(request, ""))
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := svc.Register(ctx)
	if err == nil || !errors.Is(err, errMissingUsableSecurityServer) {
		t.Fatalf("Register error = %v, want missing usable Security-Server offer", err)
	}
}
