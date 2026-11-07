package imscore

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/iniwex5/vowifi-go/internal/vowifi/ipsec3gpp"
)

// switchIPSecNetwork records the alongside-install lifecycle and hands each
// protected dial to a scripted P-CSCF.
type switchIPSecNetwork struct {
	*loopbackIPSecNetwork
	mu                       sync.Mutex
	added                    []ipsec3gpp.Policy
	aborts, commits, retires int
	addErr                   error
	dialLocal                *net.TCPAddr
	pcscf                    net.Conn
	sm7                      chan string
	sm7Status                int
	sm7Headers               string
	beforeAnswer             func()
}

func (n *switchIPSecNetwork) AddIPSec3GPP(policy ipsec3gpp.Policy) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.addErr != nil {
		return n.addErr
	}
	n.added = append(n.added, policy)
	return nil
}

func (n *switchIPSecNetwork) AbortAddedIPSec3GPP() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.aborts++
	return nil
}

func (n *switchIPSecNetwork) CommitAddedIPSec3GPP() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.commits++
	return nil
}

func (n *switchIPSecNetwork) RetirePreviousIPSec3GPP() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.retires++
	return nil
}

func (n *switchIPSecNetwork) counts() (int, int, int, int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.added), n.aborts, n.commits, n.retires
}

// DialTCPContext answers SM7 with sm7Status, or closes the flow without
// answering when sm7Status is 0.
func (n *switchIPSecNetwork) DialTCPContext(_ context.Context, local, remote *net.TCPAddr) (net.Conn, error) {
	pipe, pcscf := net.Pipe()
	ue := addressedConn{Conn: pipe, local: local, remote: remote}
	n.mu.Lock()
	n.dialLocal, n.pcscf = local, pcscf
	n.mu.Unlock()
	go func() {
		if n.sm7Status < 0 {
			pcscf.Close()
			n.sm7 <- ""
			return
		}
		request, err := readSIPStreamMessage(bufio.NewReader(pcscf))
		if err != nil {
			pcscf.Close()
			return
		}
		n.sm7 <- request
		if n.beforeAnswer != nil {
			n.beforeAnswer()
		}
		if n.sm7Status == 0 {
			pcscf.Close()
			return
		}
		_, _ = pcscf.Write([]byte(registerWireResponse(request, n.sm7Status, n.sm7Headers)))
		_, _ = io.Copy(io.Discard, pcscf)
	}()
	return ue, nil
}

// addressedConn gives a pipe the dialled endpoints.
type addressedConn struct {
	net.Conn
	local, remote net.Addr
}

func (conn addressedConn) LocalAddr() net.Addr { return conn.local }

// Write reports a reset peer the way a real socket does.
func (conn addressedConn) Write(data []byte) (int, error) {
	n, err := conn.Conn.Write(data)
	if errors.Is(err, io.ErrClosedPipe) {
		err = &net.OpError{Op: "write", Net: "tcp", Err: syscall.ECONNRESET}
	}
	return n, err
}
func (conn addressedConn) RemoteAddr() net.Addr { return conn.remote }

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func pipeClosed(conn net.Conn) bool {
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	_, err := conn.Read(make([]byte, 1))
	return err != nil && !errors.Is(err, os.ErrDeadlineExceeded)
}

const switchedSecurityServer = "ipsec-3gpp;alg=hmac-sha-1-96;ealg=aes-cbc;prot=esp;mod=trans;spi-c=3001;spi-s=3002;port-c=16382;port-s=15060"

// switchRefreshService is an established protected registration whose flow
// runs over oldFlow; SM1 is answered with a challenge carrying a new
// Security-Server, SM7 goes to the scripted P-CSCF.
func switchRefreshService(t *testing.T, sm7Status int, securityServer string) (*Service, *switchIPSecNetwork, *registerSession, net.Conn, *[]string) {
	t.Helper()
	svc, loopback, previous := establishedRefreshService(t, true)
	network := &switchIPSecNetwork{loopbackIPSecNetwork: loopback, sm7: make(chan string, 2), sm7Status: sm7Status}
	svc.cfg.IMSNetwork = network
	oldFlow, oldPeer := net.Pipe()
	t.Cleanup(func() { oldFlow.Close(); oldPeer.Close() })
	svc.mu.Lock()
	svc.registrationTransport, svc.registrationTCP, svc.registrationTCPProtected = "tcp", oldFlow, true
	svc.regState, svc.signalingReady = regRegistered, true
	svc.lastRegisterOKAt = time.Now()
	svc.mu.Unlock()
	var sm1 []string
	captureRegisterRequests(svc, func(request string, _ int) *sipResponse {
		sm1 = append(sm1, request)
		return akaChallengeResponse(request, securityServer)
	})
	previousGrace := refreshSARetireGrace
	refreshSARetireGrace = 10 * time.Millisecond
	t.Cleanup(func() { refreshSARetireGrace = previousGrace })
	return svc, network, previous, oldPeer, &sm1
}

func receiveSM7(t *testing.T, network *switchIPSecNetwork) string {
	t.Helper()
	select {
	case request := <-network.sm7:
		return request
	case <-time.After(3 * time.Second):
		t.Fatal("SM7 was not sent over the new flow")
		return ""
	}
}

// TS 33.203 7.4.1a / TS 24.229 5.1.1.5.1: the UE creates the new SAs from the
// offered Security-Client and the new Security-Server, sends SM7 over them
// from the new port_uc with the same Security-Client, Security-Verify set to
// the new Security-Server and the same Call-ID, while other traffic stays on
// the old flow; after the 200 the new SAs carry further requests and the old
// ones are deleted once a message has arrived over the new SAs.
func TestChallengedRefreshSwitchesToNewSAs(t *testing.T) {
	svc, network, previous, oldPeer, sm1 := switchRefreshService(t, 200, switchedSecurityServer)
	svc.mu.RLock()
	oldFlow := svc.registrationTCP
	svc.mu.RUnlock()
	if _, _, err := svc.runRegisterAttempt(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	sm7 := receiveSM7(t, network)
	svc.mu.RLock()
	session, flow := svc.regSession, svc.registrationTCP
	svc.mu.RUnlock()
	offer := session.refreshOffer
	if len(*sm1) != 1 || offer == nil {
		t.Fatalf("SM1 sent %d times, offer %v", len(*sm1), offer)
	}
	added, aborts, commits, retires := network.counts()
	if added != 1 || aborts != 0 || commits != 1 || retires != 0 {
		t.Fatalf("added %d aborts %d commits %d retires %d", added, aborts, commits, retires)
	}
	policy := network.added[0]
	if policy.LocalPortC != int(offer.client.PortC) || policy.LocalPortS != int(previous.security.client.PortS) ||
		policy.RemotePortC != 16382 || policy.RemotePortS != 15060 ||
		policy.FlowC.InboundSPI != offer.client.SPIC || policy.FlowS.InboundSPI != offer.client.SPIS ||
		policy.FlowC.OutboundSPI != 3002 || policy.FlowS.OutboundSPI != 3001 {
		t.Fatalf("new SA policy = %+v", policy)
	}
	if network.dialLocal == nil || network.dialLocal.Port != int(offer.client.PortC) {
		t.Fatalf("SM7 flow dialled from %v, want port_uc %d", network.dialLocal, offer.client.PortC)
	}
	if sipHeaderValue(sm7, "Security-Client") != sipHeaderValue((*sm1)[0], "Security-Client") ||
		sipHeaderValue(sm7, "Security-Client") != offer.header {
		t.Fatal("SM7 Security-Client differs from the challenged REGISTER")
	}
	if got := sipHeaderValue(sm7, "Security-Verify"); got != switchedSecurityServer {
		t.Fatalf("SM7 Security-Verify = %q", got)
	}
	if sipHeaderValue(sm7, "Call-ID") != sipHeaderValue((*sm1)[0], "Call-ID") || sipHeaderValue(sm7, "Authorization") == "" {
		t.Fatal("SM7 changed the Call-ID or omitted Authorization")
	}
	if got := sipHeaderValue((*sm1)[0], "Security-Verify"); got != previous.security.verifyHeader {
		t.Fatalf("SM1 Security-Verify = %q", got)
	}
	if session.security.client != offer.client || session.security.server.PortC != 16382 ||
		session.security.verifyHeader != switchedSecurityServer || session.security.clientHeader != offer.header {
		t.Fatalf("active security agreement after the switch = %+v", session.security)
	}
	svc.mu.RLock()
	spiPairs, verify, clientPort := svc.spiPairs, svc.securityVerify, svc.protectedClientPort
	svc.mu.RUnlock()
	if len(spiPairs) != 2 || spiPairs[0] != [2]uint32{offer.client.SPIC, 3002} || spiPairs[1] != [2]uint32{offer.client.SPIS, 3001} ||
		verify != switchedSecurityServer || clientPort != int(offer.client.PortC) {
		t.Fatalf("service security state spi %v verify %q port-c %d", spiPairs, verify, clientPort)
	}
	if flow == oldFlow || flow == nil {
		t.Fatal("further requests are not sent over the new flow after the 200")
	}
	if pipeClosed(oldPeer) {
		t.Fatal("the old flow was closed before any message arrived over the new SAs")
	}
	network.mu.Lock()
	pcscf := network.pcscf
	network.mu.Unlock()
	options := "OPTIONS sip:ue@10.0.0.2 SIP/2.0\r\nVia: SIP/2.0/TCP 10.0.0.1:15060;branch=z9hG4bKnew\r\n" +
		"From: <sip:pcscf@ims.example>;tag=p\r\nTo: <sip:ue@ims.example>\r\nCall-ID: new-sa\r\nCSeq: 1 OPTIONS\r\n" +
		"Content-Length: 0\r\n\r\n"
	_, _ = pcscf.Write([]byte(options))
	waitFor(t, "the old SAs were not retired after a message over the new SAs", func() bool {
		_, _, _, retires := network.counts()
		return retires == 1
	})
	if !pipeClosed(oldPeer) {
		t.Fatal("the old flow stayed open after the old SAs were retired")
	}
	if network.installs != 0 || network.removals != 0 {
		t.Fatalf("switch replaced the SAs instead of adding: installs %d removals %d", network.installs, network.removals)
	}
}

// TS 33.203 7.4.1a: the old SAs are always deleted when their lifetime ends,
// and with the service.
func TestReplacedSAsRetireAtLifetimeEndAndOnStop(t *testing.T) {
	svc, network, _, oldPeer, _ := switchRefreshService(t, 200, switchedSecurityServer)
	svc.mu.Lock()
	svc.lastRegisterOKAt = time.Time{}
	svc.mu.Unlock()
	if _, _, err := svc.runRegisterAttempt(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	waitFor(t, "the old SAs outlived their lifetime", func() bool {
		_, _, _, retires := network.counts()
		return retires == 1
	})
	if !pipeClosed(oldPeer) {
		t.Fatal("the old flow outlived its SAs")
	}

	svc, network, _, oldPeer, _ = switchRefreshService(t, 200, switchedSecurityServer)
	if _, _, err := svc.runRegisterAttempt(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	svc.StopCurrent()
	if _, _, _, retires := network.counts(); retires != 1 || !pipeClosed(oldPeer) {
		t.Fatalf("stop left the replaced SAs installed: retires %d", retires)
	}
}

// TS 24.229 5.1.1.5.1: on a 403 or no 200 to SM7 the UE deletes the temporary
// SAs and uses the old set. Keeping the binding for a retry is our choice.
func TestFailedSM7RestoresTheOldSAs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		headers string
	}{
		{"503", 503, ""},
		{"403", 403, ""},
		{"500 IP+Port conflict", 500, "Warning: 399 pcscf \"AKA IP+Port conflict\"\r\n"},
		{"second challenge without Security-Server", 401, "WWW-Authenticate: " + `Digest realm="ims.example", nonce="EREREREREREREREREREREiIiIiIiIiIiIiIiIiIiIg==", algorithm=AKAv1-MD5, qop="auth"` + "\r\n"},
		{"second challenge", 401, "WWW-Authenticate: " + `Digest realm="ims.example", nonce="EREREREREREREREREREREiIiIiIiIiIiIiIiIiIiIg==", algorithm=AKAv1-MD5, qop="auth"` + "\r\nSecurity-Server: " + switchedSecurityServer + "\r\n"},
		{"flow closed without a response", 0, ""},
		{"SM7 write failed", -1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, network, previous, oldPeer, _ := switchRefreshService(t, tc.status, switchedSecurityServer)
			network.sm7Headers = tc.headers
			svc.mu.RLock()
			oldFlow := svc.registrationTCP
			svc.mu.RUnlock()
			started := time.Now()
			_, _, err := svc.runRegisterAttempt(context.Background())
			if err == nil {
				t.Fatal("a failed SM7 completed the refresh")
			}
			if elapsed := time.Since(started); elapsed > 3*time.Second {
				t.Fatalf("a failed SM7 held the refresh for %v", elapsed)
			}
			receiveSM7(t, network)
			added, aborts, commits, _ := network.counts()
			if added != 1 || aborts != 1 || commits != 0 {
				t.Fatalf("added %d aborts %d commits %d", added, aborts, commits)
			}
			svc.mu.RLock()
			session, flow, pending, state := svc.regSession, svc.registrationTCP, svc.registrationPendingTCP, svc.regState
			svc.mu.RUnlock()
			if session.security != previous.security || flow != oldFlow || pending != nil || state != regRegistered {
				t.Fatalf("a failed SM7 left the old SAs or flow: state %v", state)
			}
			if pipeClosed(oldPeer) {
				t.Fatal("a failed SM7 closed the old flow")
			}
			select {
			case reported := <-svc.registerErrors:
				t.Fatalf("a failed SM7 requested a runtime reconnect: %v", reported)
			default:
			}
			if !svc.keepRegistrationAfterFailedRefresh(true, err) {
				t.Fatalf("a failed re-authentication dropped the binding: %v", err)
			}
		})
	}
}

func TestRefreshSwitchLocalFailuresKeepTheOldSAs(t *testing.T) {
	t.Run("install", func(t *testing.T) {
		svc, network, previous, _, _ := switchRefreshService(t, 200, switchedSecurityServer)
		network.addErr = errors.New("installer refused")
		_, _, err := svc.runRegisterAttempt(context.Background())
		var failed *refreshSwitchFailed
		if !errors.As(err, &failed) || failed.phase != "install" {
			t.Fatalf("err = %v", err)
		}
		if network.dialLocal != nil || svc.regSession.security != previous.security {
			t.Fatal("a failed install dialled or replaced the active SAs")
		}
		if !svc.keepRegistrationAfterFailedRefresh(true, err) {
			t.Fatal("a local switch failure dropped the binding")
		}
	})
}

// The protected UDP path follows the switch: sends leave from the new port_uc
// to port_ps and requests from the new port_pc are accepted on the unchanged
// port_us socket, while the old port_pc keeps its path until the old SAs are
// deleted (TS 33.203 7.4.0, 7.4.1a).
func TestRefreshSwitchMovesProtectedUDPToNewPortC(t *testing.T) {
	svc, network, _, _, _ := switchRefreshService(t, 200, switchedSecurityServer)
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	oldClient, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer oldClient.Close()
	pcscf := net.IPv4(10, 0, 0, 1)
	oldPath := &protectedUDPTransport{
		client: oldClient, server: server, registrar: svc.registrar,
		remoteClient: &net.UDPAddr{IP: pcscf, Port: 16381}, remoteServer: &net.UDPAddr{IP: pcscf, Port: 15060},
	}
	svc.mu.Lock()
	svc.protectedUDP = oldPath
	svc.mu.Unlock()
	if _, _, err := svc.runRegisterAttempt(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	receiveSM7(t, network)
	svc.mu.RLock()
	path, offer := svc.protectedUDP, svc.regSession.refreshOffer
	svc.mu.RUnlock()
	if path == oldPath || path.server != server || tcpPortFromAddr(path.client.LocalAddr()) != int(offer.client.PortC) ||
		path.remoteClient.Port != 16382 || path.remoteServer.Port != 15060 {
		t.Fatalf("protected UDP after the switch = %+v", path)
	}
	if svc.protectedUDPFor(oldPath, &net.UDPAddr{IP: pcscf, Port: 16382}) != path ||
		svc.protectedUDPFor(oldPath, &net.UDPAddr{IP: pcscf, Port: 16381}) != oldPath {
		t.Fatal("the port_us receiver does not route by the sending port_pc")
	}
	svc.retireReplacedSecurity(svc.saRetirement.Load(), "test")
	if _, err := oldClient.WriteTo([]byte("x"), server.LocalAddr()); err == nil {
		t.Fatal("the old port_uc UDP socket was not closed at retirement")
	}
	if svc.protectedUDPFor(oldPath, &net.UDPAddr{IP: pcscf, Port: 16381}) != path {
		t.Fatal("the old path outlived its SAs")
	}
	if _, err := path.client.WriteTo([]byte("x"), server.LocalAddr()); err != nil {
		t.Fatalf("the new port_uc UDP socket was closed: %v", err)
	}
}

// A close of the old flow while SM7 is outstanding ends neither SM7 nor the
// registration: the old flow is not needed once the new SAs are taken into
// use (TS 33.203 7.4.1a), and the switch decides the outcome.
func TestOldFlowCloseDuringSwitchLeavesTheOutcomeToSM7(t *testing.T) {
	svc, network, _, oldPeer, _ := switchRefreshService(t, 200, switchedSecurityServer)
	svc.mu.RLock()
	oldFlow := svc.registrationTCP
	svc.mu.RUnlock()
	svc.networkDone.Add(1)
	go svc.readRegistrationStream(oldFlow)
	handled := make(chan bool, 1)
	network.beforeAnswer = func() {
		oldPeer.Close()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			svc.mu.RLock()
			cleared := svc.registrationTCP == nil
			svc.mu.RUnlock()
			if cleared {
				handled <- true
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		handled <- false
	}
	if _, _, err := svc.runRegisterAttempt(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !<-handled {
		t.Fatal("the old flow close was not handled before SM12")
	}
	svc.mu.RLock()
	flow, state := svc.registrationTCP, svc.regState
	svc.mu.RUnlock()
	if _, _, commits, _ := network.counts(); commits != 1 || flow == nil || state != regRegistered {
		t.Fatalf("commits %d flow %v state %v", commits, flow, state)
	}
	select {
	case reported := <-svc.registerErrors:
		t.Fatalf("the old flow close requested a runtime reconnect: %v", reported)
	default:
	}
}

// After the 200 new requests use the new SAs (TS 33.203 7.4.1a): an
// established dialog stops pinning the old flow's sender.
func TestRefreshSwitchMovesDialogsToTheNewFlow(t *testing.T) {
	svc, _, _, _, _ := switchRefreshService(t, 200, switchedSecurityServer)
	dialog := &imscoreDialogHandle{id: "call-1", sender: func(string) error { return nil }}
	svc.dialogs().store(dialog)
	if _, _, err := svc.runRegisterAttempt(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	dialog.mu.Lock()
	pinned := dialog.sender != nil
	dialog.mu.Unlock()
	if pinned {
		t.Fatal("an established dialog still sends over the old flow")
	}
}

// TS 33.203 7.4.1a: after a message over the new SAs the old ones go once
// no transaction is pending, and at their lifetime end in any case.
func TestReplacedSAsWaitForPendingTransactionsUntilLifetimeEnd(t *testing.T) {
	svc, network, _, _, _ := switchRefreshService(t, 200, switchedSecurityServer)
	if _, _, err := svc.runRegisterAttempt(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	retirement := svc.saRetirement.Load()
	retirement.timer.Stop()
	svc.transport.mu.Lock()
	svc.transport.waiters[sipTransactionKey{}] = &clientSIPTransaction{}
	svc.transport.mu.Unlock()
	svc.retireWhenDrained(retirement)
	if _, _, _, retires := network.counts(); retires != 0 || svc.saRetirement.Load() != retirement {
		t.Fatal("retired the old SAs while a transaction was pending")
	}
	retirement.timer.Stop()
	retirement.lifetimeEnd = time.Now().Add(-time.Millisecond)
	svc.retireWhenDrained(retirement)
	if _, _, _, retires := network.counts(); retires != 1 {
		t.Fatal("kept the old SAs past their lifetime")
	}
	svc.transport.mu.Lock()
	delete(svc.transport.waiters, sipTransactionKey{})
	svc.transport.mu.Unlock()
}

// The new flow closing between SM12 and the commit leaves no flow: the
// refresh fails into a fresh registration instead of claiming success.
func TestNewFlowClosedBeforeCommitFailsTheRefresh(t *testing.T) {
	svc, network, _, _, _ := switchRefreshService(t, 200, switchedSecurityServer)
	network.beforeAnswer = func() {
		svc.mu.Lock()
		svc.registrationPendingErr = io.EOF
		svc.mu.Unlock()
	}
	_, _, err := svc.runRegisterAttempt(context.Background())
	if err == nil || !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v", err)
	}
	svc.mu.RLock()
	flow := svc.registrationTCP
	svc.mu.RUnlock()
	if flow != nil || svc.keepRegistrationAfterFailedRefresh(true, err) {
		t.Fatal("a refresh without a live flow was kept as registered")
	}
	select {
	case reported := <-svc.registerErrors:
		t.Fatalf("requested a runtime reconnect instead of re-registering: %v", reported)
	default:
	}
}

// A port-s flow left on the old SAs is closed when they are deleted; the
// P-CSCF reopens port-s over the new SAs when it next sends (TS 24.229
// 5.2.2.2), so the UE waits for it instead of treating port-s as lost.
func TestRetiredPortSFlowAwaitsTheNewSAs(t *testing.T) {
	svc, _, _, _, _ := switchRefreshService(t, 200, switchedSecurityServer)
	if _, _, err := svc.runRegisterAttempt(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	pipe, pcscf := net.Pipe()
	defer pcscf.Close()
	oldPortS := addressedConn{Conn: pipe,
		local:  &net.TCPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 41001},
		remote: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 16381}}
	if !svc.trackProtectedPortSConnection(oldPortS, time.Now()) {
		t.Fatal("port-s flow not tracked")
	}
	svc.networkDone.Add(1)
	go svc.serveProtectedSIPConnection(oldPortS)
	svc.retireReplacedSecurity(svc.saRetirement.Load(), "test")
	waitFor(t, "the retired port-s close was not recorded", func() bool {
		return svc.capturePortSSession().lastCloseKind == portSCloseSARetired
	})
	if !svc.canAwaitOnDemandPortS() {
		t.Fatal("a port-s flow closed with its SAs is not awaited from the P-CSCF")
	}
	svc.mu.RLock()
	pushRequired := svc.protectedSMSPushRequiredLocked()
	svc.mu.RUnlock()
	if pushRequired {
		t.Fatal("SMS readiness requires the retired port-s flow")
	}
}

// The P-CSCF may delete the old SAs first (TS 33.203 7.4.2a) and reset or
// close its flow from the old port_pc. That is the same retirement; a reset
// from the new port_pc, or our own timeout on the old one, is still a lost
// port-s.
func TestPCSCFResetOfOldSAPortSAwaitsTheNewSAs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		remotePort int
		err        error
		want       string
	}{
		{"reset from old port_pc", 16381, syscall.ECONNRESET, portSCloseSARetired},
		{"close from old port_pc", 16381, io.EOF, portSCloseSARetired},
		{"timeout on old port_pc", 16381, syscall.ETIMEDOUT, portSCloseTimeout},
		{"reset from new port_pc", 16382, syscall.ECONNRESET, portSClosePeerReset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, _, _, _ := switchRefreshService(t, 200, switchedSecurityServer)
			if _, _, err := svc.runRegisterAttempt(context.Background()); err != nil {
				t.Fatalf("refresh: %v", err)
			}
			if retirement := svc.saRetirement.Load(); retirement == nil ||
				retirement.oldServerPortC != 16381 || retirement.newServerPortC != 16382 {
				t.Fatalf("retirement = %+v", retirement)
			}
			pipe, pcscf := net.Pipe()
			defer pcscf.Close()
			portS := addressedConn{Conn: pipe,
				local:  &net.TCPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 41001},
				remote: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: tc.remotePort}}
			if !svc.trackProtectedPortSConnection(portS, time.Now()) {
				t.Fatal("port-s flow not tracked")
			}
			svc.recordPortSClosed(portS, tc.err, time.Now())
			if got := svc.capturePortSSession().lastCloseKind; got != tc.want {
				t.Fatalf("close kind = %q, want %q", got, tc.want)
			}
			if tc.want != portSCloseSARetired {
				return
			}
			if !svc.awaitPortSAfterRegister() {
				t.Fatal("a port-s flow reset with the old SAs is not awaited from the P-CSCF")
			}
			if count := svc.capturePortSSession().peerResetCount; count != 0 {
				t.Fatalf("peer resets = %d, want 0", count)
			}
			svc.mu.RLock()
			pushRequired := svc.protectedSMSPushRequiredLocked()
			svc.mu.RUnlock()
			if pushRequired {
				t.Fatal("SMS readiness requires the port-s flow reset with the old SAs")
			}
		})
	}
}

// A failure of the port-s flow over the new SAs is kept when the old flow
// goes with the old SAs afterwards.
func TestOldSAPortSResetKeepsNewFlowFailure(t *testing.T) {
	svc, _, _, _, _ := switchRefreshService(t, 200, switchedSecurityServer)
	if _, _, err := svc.runRegisterAttempt(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	portS := func(remotePort int) net.Conn {
		pipe, pcscf := net.Pipe()
		t.Cleanup(func() { pcscf.Close() })
		conn := addressedConn{Conn: pipe,
			local:  &net.TCPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 41001},
			remote: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: remotePort}}
		if !svc.trackProtectedPortSConnection(conn, time.Now()) {
			t.Fatal("port-s flow not tracked")
		}
		return conn
	}
	oldFlow, newFlow := portS(16381), portS(16382)
	svc.recordPortSClosed(newFlow, syscall.ECONNRESET, time.Now())
	svc.recordPortSClosed(oldFlow, syscall.ECONNRESET, time.Now())
	if got := svc.capturePortSSession().lastCloseKind; got != portSClosePeerReset {
		t.Fatalf("close kind = %q, want the new flow's %q", got, portSClosePeerReset)
	}
}

type closeHookConn struct {
	addressedConn
	onClose func()
}

func (conn *closeHookConn) Close() error {
	conn.onClose()
	return conn.addressedConn.Close()
}

// A P-CSCF reset of an old flow that lands while the UE is retiring the old
// SAs is still the old SAs going.
func TestOldSAPortSResetDuringRetirementAwaitsTheNewSAs(t *testing.T) {
	svc, _, _, _, _ := switchRefreshService(t, 200, switchedSecurityServer)
	if _, _, err := svc.runRegisterAttempt(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	oldAddr := func(local int) (addressedConn, net.Conn) {
		pipe, pcscf := net.Pipe()
		return addressedConn{Conn: pipe,
			local:  &net.TCPAddr{IP: net.IPv4(10, 0, 0, 2), Port: local},
			remote: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 16381}}, pcscf
	}
	reset, resetPeer := oldAddr(41001)
	defer resetPeer.Close()
	swept, sweptPeer := oldAddr(41002)
	defer sweptPeer.Close()
	hooked := &closeHookConn{addressedConn: swept, onClose: func() {
		svc.recordPortSClosed(reset, syscall.ECONNRESET, time.Now())
	}}
	if !svc.trackProtectedPortSConnection(hooked, time.Now()) {
		t.Fatal("port-s flow not tracked")
	}
	// The reset flow is the latest one but already left protectedConns, so
	// retirement never sweeps it.
	svc.recordPortSOpened(reset, time.Now())
	svc.retireReplacedSecurity(svc.saRetirement.Load(), "test")
	if got := svc.capturePortSSession().lastCloseKind; got != portSCloseSARetired {
		t.Fatalf("close kind = %q, want %q", got, portSCloseSARetired)
	}
	if svc.saRetirement.Load() != nil {
		t.Fatal("the retirement was not cleared after cleanup")
	}
}

// A retire timer re-armed while the retirement was being cleaned up must not
// keep waiting for pending transactions afterwards.
func TestRetiredRetirementTimerIsANoOp(t *testing.T) {
	svc, _, _, _, _ := switchRefreshService(t, 200, switchedSecurityServer)
	if _, _, err := svc.runRegisterAttempt(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	retirement := svc.saRetirement.Load()
	svc.retireReplacedSecurity(retirement, "test")
	svc.transport.mu.Lock()
	svc.transport.waiters[sipTransactionKey{}] = &clientSIPTransaction{}
	svc.transport.mu.Unlock()
	defer func() {
		svc.transport.mu.Lock()
		delete(svc.transport.waiters, sipTransactionKey{})
		svc.transport.mu.Unlock()
	}()
	svc.retireWhenDrained(retirement)
	if retirement.timer.Stop() {
		t.Fatal("a retired retirement re-armed its timer")
	}
}

// Retiring the old SAs closes the old port-s flow; when a newer flow has
// already failed, that failure stays the one recovery acts on.
func TestRetiredOldFlowKeepsNewFlowFailure(t *testing.T) {
	svc, _, _, _, _ := switchRefreshService(t, 200, switchedSecurityServer)
	if _, _, err := svc.runRegisterAttempt(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	portS := func(remotePort int) net.Conn {
		pipe, pcscf := net.Pipe()
		t.Cleanup(func() { pcscf.Close() })
		conn := addressedConn{Conn: pipe,
			local:  &net.TCPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 41001},
			remote: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: remotePort}}
		if !svc.trackProtectedPortSConnection(conn, time.Now()) {
			t.Fatal("port-s flow not tracked")
		}
		return conn
	}
	oldFlow, newFlow := portS(16381), portS(16382)
	svc.recordPortSClosed(newFlow, syscall.ECONNRESET, time.Now())
	svc.markPortSSARetired(oldFlow)
	svc.recordPortSClosed(oldFlow, io.EOF, time.Now())
	if got := svc.capturePortSSession().lastCloseKind; got != portSClosePeerReset {
		t.Fatalf("close kind = %q, want the new flow's %q", got, portSClosePeerReset)
	}
}
