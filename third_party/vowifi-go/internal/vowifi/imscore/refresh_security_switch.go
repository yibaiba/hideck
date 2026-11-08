package imscore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/iniwex5/vowifi-go/internal/vowifi/ipsec3gpp"
	"github.com/iniwex5/vowifi-go/internal/vowifi/logging"
)

// TS 33.203 7.4.1a for a refresh REGISTER that the S-CSCF re-authenticates:
// SM1 and SM6 use the old SAs; the UE then creates the new SAs from the
// offered Security-Client and the received Security-Server and sends SM7 over
// the new outbound SA, while all other traffic stays on the old SAs until
// SM12 (the 200). After SM12 new requests use the new SAs; transactions
// started on the old SAs may finish there. The old SAs are deleted once a
// further message arrives over the new SAs and the pending transactions are
// done, and always when their lifetime expires (TS 24.229 5.1.1.5.1). On a
// failed re-authentication the new SAs are deleted and the old ones stay.

// refreshSARetireGrace covers transactions still pending on the old SAs once
// a message has arrived over the new ones: Timer B/F, 64*T1 (RFC 3261).
var refreshSARetireGrace = 64 * 500 * time.Millisecond

// refreshSALifetimeMargin is added to the old registration's expiry, as the
// SIP level SA lifetime is (TS 24.229 5.1.1.5.1).
const refreshSALifetimeMargin = 30 * time.Second

type ipsec3gppSwitcher interface {
	AddIPSec3GPP(ipsec3gpp.Policy) error
	AbortAddedIPSec3GPP() error
	CommitAddedIPSec3GPP() error
	RetirePreviousIPSec3GPP() error
}

// pendingSecuritySwitch is the new SA set installed for SM7 and the flow
// that carries SM7. Both are owned by the attempt until the 200.
type pendingSecuritySwitch struct {
	server         *securityMechanism
	verify         string
	switcher       ipsec3gppSwitcher
	conn           net.Conn
	sm7Ctx         context.Context
	cancelSM7      context.CancelFunc
	sendErr        atomic.Pointer[error]
	oldLifetimeEnd time.Time
	started        time.Time
	done           bool
}

// saRetirement is the replaced SA set after a commit, with the flows it
// protects.
type saRetirement struct {
	switcher       ipsec3gppSwitcher
	oldConn        net.Conn
	oldUDP         *protectedUDPTransport
	oldServerPortC int
	newClientPortC int
	newServerPortC int
	committedAt    time.Time
	lifetimeEnd    time.Time
	timer          *time.Timer
	newSAInbound   atomic.Bool
}

// refreshSwitchFailed is a local failure, or SM7 without any response,
// while switching. The old SAs and the binding are kept, as after a declined
// challenge.
type refreshSwitchFailed struct {
	phase string
	err   error
}

func (err *refreshSwitchFailed) Error() string {
	return fmt.Sprintf("imscore: refresh SA switch failed at %s: %v", err.phase, err.err)
}

func (err *refreshSwitchFailed) Unwrap() error { return err.err }

func (s *Service) refreshSecuritySwitcher() (ipsec3gppSwitcher, bool) {
	if s == nil || s.cfg == nil || s.cfg.IMSNetwork == nil {
		return nil, false
	}
	switcher, ok := s.cfg.IMSNetwork.(ipsec3gppSwitcher)
	return switcher, ok
}

// canSwitchRefreshSecurity reports whether a refresh challenge carrying a
// Security-Server can be answered over new SAs. A challenge to SM7 itself is
// not: one registration procedure at a time (TS 33.203 7.4.1a).
func (s *Service) canSwitchRefreshSecurity(session *registerSession, response *sipResponse) bool {
	if session.refreshOffer == nil || session.switching != nil || len(response.HeaderValues("Security-Server")) == 0 {
		return false
	}
	_, ok := s.refreshSecuritySwitcher()
	return ok
}

// refreshSwitchRefusal checks a Security-Server before AKA: one that cannot
// build the new set is refused without using the challenge (TS 24.229
// 5.1.1.5.1 item 3).
func (s *Service) refreshSwitchRefusal(session *registerSession, challenge registerSecurityChallenge) error {
	active := session.security
	server, decision := challenge.server, challenge.decision
	s.mu.RLock()
	external := s.externalTransport
	protectedUDP := s.protectedUDP != nil
	s.mu.RUnlock()
	switch {
	case external:
		return errors.New("an external transport carries the registration flow")
	case decision.err != nil:
		return decision.err
	case !decision.useIPSec || server == nil:
		return fmt.Errorf("security challenge decided %q without IPsec", decision.mode)
	case server.PortC == active.server.PortC:
		// Both sets would select port_us -> port_pc. TS 33.203 7.4.0 has the
		// P-CSCF change port_pc and keep port_ps; refusing otherwise is our
		// choice.
		return fmt.Errorf("P-CSCF kept port-c %d", server.PortC)
	case server.PortS != active.server.PortS:
		return fmt.Errorf("P-CSCF moved port-s %d -> %d", active.server.PortS, server.PortS)
	case protectedUDP && session.refreshOffer.udp == nil:
		return errors.New("no UDP reservation for the new port-c")
	}
	return nil
}

// switchRefreshSecurity installs the new SAs alongside the active ones and
// opens the SM7 flow from the offered port_uc without publishing it.
func (s *Service) switchRefreshSecurity(ctx context.Context, session *registerSession, response *sipResponse, aka AKAResult) error {
	offer := session.refreshOffer
	active := session.security
	switcher, _ := s.refreshSecuritySwitcher()
	challenge := s.evaluateRegisterSecurityChallenge(response, session.template)
	server, verify := challenge.server, challenge.verify
	fail := func(phase string, err error) error {
		logging.Info("IMS refresh SA switch failed; keeping the active SAs",
			"device", s.DeviceID(), "cseq", session.cseq, "phase", phase, "err", err,
			"security_server", logging.RedactSIPRaw(strings.Join(response.HeaderValues("Security-Server"), ",")))
		return &refreshSwitchFailed{phase: phase, err: err}
	}
	if err := s.refreshSwitchRefusal(session, challenge); err != nil {
		return fail("negotiate", err)
	}
	s.mu.RLock()
	lastOK := s.lastRegisterOKAt
	s.mu.RUnlock()
	remote := s.currentRegistrationRemote()
	if remote == nil || remote.IP == nil {
		return fail("negotiate", errors.New("registrar address is unavailable"))
	}
	// A set replaced by an earlier switch goes before the next is added.
	s.retireReplacedSecurity(s.saRetirement.Load(), "next_switch")
	logging.Info("IMS refresh re-authentication offers new SAs",
		"device", s.DeviceID(), "cseq", session.cseq,
		"server_port_c", active.server.PortC, "new_server_port_c", server.PortC, "server_port_s", server.PortS,
		"server_spi_c", active.server.SPIC, "new_server_spi_c", server.SPIC,
		"server_spi_s", active.server.SPIS, "new_server_spi_s", server.SPIS,
		"auth", server.Auth, "encryption", server.Encryption)
	policy := negotiatedIPSecPolicy(s.cfg.LocalIP, remote.IP, offer.client, *server, aka)
	if err := switcher.AddIPSec3GPP(policy); err != nil {
		return fail("install", err)
	}
	started := time.Now()
	abort := func(phase string, err error) error {
		if abortErr := switcher.AbortAddedIPSec3GPP(); abortErr != nil {
			err = errors.Join(err, fmt.Errorf("imscore: remove new SAs: %w", abortErr))
		}
		return fail(phase, err)
	}
	// The reserved listener holds port_uc until the dial binds it.
	tcpErr := offer.tcp.Close()
	offer.tcp = nil
	if tcpErr != nil {
		return abort("dial", fmt.Errorf("imscore: release reserved port-c %d: %w", offer.client.PortC, tcpErr))
	}
	conn, err := s.dialProtectedTCP(ctx, offer.client, *server)
	if err != nil {
		return abort("dial", err)
	}
	s.configureRegistrationTCPKeepalive(conn)
	conn = s.newInboundCountingConn(conn)
	s.mu.Lock()
	if s.stopped() {
		s.mu.Unlock()
		_ = conn.Close()
		return abort("dial", net.ErrClosed)
	}
	sm7Ctx, cancelSM7 := context.WithCancel(ctx)
	s.registrationPendingTCP, s.registrationPendingCancel, s.registrationPendingErr = conn, cancelSM7, nil
	s.networkDone.Add(1)
	s.mu.Unlock()
	go s.readRegistrationStream(conn)
	oldLifetimeEnd := time.Now().Add(refreshSARetireGrace)
	if !lastOK.IsZero() && session.expires > 0 {
		if end := lastOK.Add(session.expires + refreshSALifetimeMargin); end.After(oldLifetimeEnd) {
			oldLifetimeEnd = end
		}
	}
	session.switching = &pendingSecuritySwitch{
		server: server, verify: verify, switcher: switcher, conn: conn,
		sm7Ctx: sm7Ctx, cancelSM7: cancelSM7, oldLifetimeEnd: oldLifetimeEnd, started: started,
	}
	logging.Info("IMS refresh SAs added alongside the active SAs",
		"device", s.DeviceID(), "cseq", session.cseq,
		"local_port_c", offer.client.PortC, "local_port_s", offer.client.PortS,
		"remote_port_c", server.PortC, "remote_port_s", server.PortS,
		"dial_ms", time.Since(started).Milliseconds(), "old_sa_lifetime_end", oldLifetimeEnd)
	return nil
}

// switchSM7Sender sends SM7 on the pending flow only; everything else still
// goes over the published flow and the old SAs.
func (s *Service) switchSM7Sender(session *registerSession) func(string) error {
	if session.switching == nil {
		return nil
	}
	pending := session.switching
	return func(request string) error {
		// A failed write ends SM7 only: returned to the transaction engine
		// it would be fatal for the published flow over the old SAs.
		if err := s.writeSIPStream(pending.conn, request); err != nil {
			pending.sendErr.Store(&err)
			pending.cancelSM7()
		}
		return nil
	}
}

// sm7Error names the write failure behind a canceled SM7.
func (pending *pendingSecuritySwitch) sm7Error(err error) error {
	if sendErr := pending.sendErr.Load(); sendErr != nil {
		return fmt.Errorf("imscore: SM7 write: %w", *sendErr)
	}
	return err
}

// commitRefreshSecuritySwitch runs on SM12: the new SAs and flow become the
// active ones; the old ones keep serving their flows until retired.
func (s *Service) commitRefreshSecuritySwitch(session *registerSession) error {
	pending := session.switching
	offer := session.refreshOffer
	if err := pending.switcher.CommitAddedIPSec3GPP(); err != nil {
		logging.Info("IMS refresh SA switch commit failed", "device", s.DeviceID(),
			"cseq", session.cseq, "err", err)
		return &refreshSwitchFailed{phase: "commit", err: err}
	}
	pending.done = true
	security := &securityAgreement{
		client: offer.client, server: pending.server,
		clientHeader: offer.header, verifyHeader: pending.verify,
	}
	retirement := &saRetirement{
		switcher: pending.switcher, oldServerPortC: int(session.security.server.PortC),
		newClientPortC: int(offer.client.PortC), newServerPortC: int(pending.server.PortC),
		committedAt: time.Now(), lifetimeEnd: pending.oldLifetimeEnd,
	}
	retirement.timer = time.AfterFunc(time.Until(pending.oldLifetimeEnd), func() {
		s.retireWhenDrained(retirement)
	})
	conn := pending.conn
	s.mu.Lock()
	pendingClosed := s.registrationPendingErr
	s.registrationPendingErr = nil
	session.security = security
	s.securityVerify = pending.verify
	s.spiPairs = [][2]uint32{
		{security.client.SPIC, security.server.SPIS},
		{security.client.SPIS, security.server.SPIC},
	}
	s.protectedClientPort = int(offer.client.PortC)
	retirement.oldConn = s.registrationTCP
	s.registrationTCP = conn
	s.registrationPendingTCP, s.registrationPendingCancel = nil, nil
	s.registrationTCPProtected = true
	s.registrationTransport = "tcp"
	s.transport.SetSendFn(func(request string) error { return s.writeSIPStream(conn, request) })
	if path := s.protectedUDP; path != nil && offer.udp != nil {
		retirement.oldUDP = path
		s.protectedUDPRetiring = path
		s.protectedUDP = &protectedUDPTransport{
			client: offer.udp, server: path.server, registrar: path.registrar,
			remoteClient: &net.UDPAddr{IP: path.remoteClient.IP, Port: int(pending.server.PortC)},
			remoteServer: &net.UDPAddr{IP: path.remoteServer.IP, Port: int(pending.server.PortS)},
		}
		offer.udp = nil
	}
	// Published with the flow swap, so StopCurrent sees one or the other.
	s.saRetirement.Store(retirement)
	s.mu.Unlock()
	pending.cancelSM7()
	// In-dialog requests follow the registration flow onto the new SAs.
	released := s.dialogs().releaseSenders()
	if s.stopped() {
		s.retireReplacedSecurity(retirement, "stop")
	}
	logging.Info("IMS security association switched",
		"device", s.DeviceID(), "cseq", session.cseq,
		"local_port_c", offer.client.PortC, "remote_port_c", pending.server.PortC,
		"old_remote_port_c", retirement.oldServerPortC,
		"elapsed_ms", time.Since(pending.started).Milliseconds(),
		"udp_moved", retirement.oldUDP != nil, "dialog_senders_released", released,
		"old_sa_lifetime_end", pending.oldLifetimeEnd, "sm7_flow_closed", errorString(pendingClosed))
	if pendingClosed != nil {
		// The new flow closed after SM12 arrived. The SAs are committed but
		// no flow is left, so the refresh fails into a fresh registration.
		s.mu.Lock()
		if s.registrationTCP == conn {
			s.registrationTCP = nil
			s.registrationTCPProtected = false
			s.transport.SetSendFn(func(string) error {
				return errors.New("imscore: registered SIP transport is not connected")
			})
		}
		s.mu.Unlock()
		// Its reader may still run; networkDone waits for it.
		_ = conn.Close()
		return fmt.Errorf("imscore: the new registration flow closed after the 200: %w", pendingClosed)
	}
	return nil
}

// noteInboundOverNewSA starts the deletion of the old SAs once a message has
// arrived over the new ones: on the new registration flow or from the new
// port_pc (TS 24.229 5.1.1.5.1). Pending old-SA transactions get the grace.
func (s *Service) noteInboundOverNewSA(localPort, remotePort int) {
	retirement := s.saRetirement.Load()
	if retirement == nil || (localPort != retirement.newClientPortC && remotePort != retirement.newServerPortC) {
		return
	}
	if retirement.newSAInbound.Swap(true) {
		return
	}
	retireIn := min(refreshSARetireGrace, time.Until(retirement.lifetimeEnd))
	retirement.timer.Reset(retireIn)
	logging.Info("IMS first message over the new SAs; old SAs retire after pending transactions",
		"device", s.DeviceID(), "local_port", localPort, "remote_port", remotePort,
		"since_switch", time.Since(retirement.committedAt).Round(time.Millisecond),
		"retire_in", retireIn.Round(time.Millisecond))
}

// retireWhenDrained retires the replaced SAs once their lifetime has ended,
// or after a message over the new SAs when no client transaction is pending
// (TS 33.203 7.4.1a); it waits again otherwise.
func (s *Service) retireWhenDrained(retirement *saRetirement) {
	// A timer re-armed while the retirement was being cleaned up.
	if s.saRetirement.Load() != retirement {
		return
	}
	remaining := time.Until(retirement.lifetimeEnd)
	if remaining <= 0 {
		s.retireReplacedSecurity(retirement, "old_sa_lifetime")
		return
	}
	if pending := s.transport.pendingClientTransactions(); pending > 0 {
		logging.Info("IMS old SAs kept for pending transactions", "device", s.DeviceID(),
			"pending_client_transactions", pending, "lifetime_left", remaining.Round(time.Second))
		retirement.timer.Reset(min(refreshSARetireGrace, remaining))
		return
	}
	s.retireReplacedSecurity(retirement, "new_sa_inbound")
}

// retireReplacedSecurity deletes the replaced SAs and closes the flows they
// protected. Only the current retirement acts; stale timers are no-ops.
func (s *Service) retireReplacedSecurity(retirement *saRetirement, trigger string) {
	// Every caller claims the retirement under saRetirementMu. Even a nil
	// caller must wait, or the previous cleanup can remove the retiring set
	// published by the next refresh switch. The pointer is cleared only after
	// cleanup, so a P-CSCF reset of an old flow racing it is still recognised
	// as the old SAs going (onReplacedSAs).
	s.saRetirementMu.Lock()
	defer s.saRetirementMu.Unlock()
	if retirement == nil || s.saRetirement.Load() != retirement {
		return
	}
	defer s.saRetirement.CompareAndSwap(retirement, nil)
	retirement.timer.Stop()
	if retirement.oldConn != nil {
		_ = retirement.oldConn.Close()
	}
	closed := 0
	for _, conn := range s.snapshotProtectedConns() {
		if tcpPortFromAddr(conn.RemoteAddr()) == retirement.oldServerPortC {
			s.markPortSSARetired(conn)
			_ = conn.Close()
			closed++
		}
	}
	var err error
	if retirement.oldUDP != nil {
		s.mu.Lock()
		if s.protectedUDPRetiring == retirement.oldUDP {
			s.protectedUDPRetiring = nil
		}
		s.mu.Unlock()
		// port_us is shared with the new path; only the old port_uc goes.
		err = retirement.oldUDP.client.Close()
	}
	err = errors.Join(err, retirement.switcher.RetirePreviousIPSec3GPP())
	logging.Info("IMS replaced security association retired",
		"device", s.DeviceID(), "trigger", trigger,
		"new_sa_inbound_seen", retirement.newSAInbound.Load(),
		"since_switch", time.Since(retirement.committedAt).Round(time.Millisecond),
		"old_port_s_conns_closed", closed, "err", errorString(err))
}

// abortRefreshSecuritySwitch deletes the new SAs and closes the SM7 flow;
// the old SAs and flow were never left (TS 33.203 7.4.1a). It is a no-op
// after a commit.
func (s *Service) abortRefreshSecuritySwitch(session *registerSession, cause error) {
	pending := session.switching
	if pending == nil || pending.done {
		return
	}
	pending.done = true
	session.switching = nil
	s.mu.Lock()
	if s.registrationPendingTCP == pending.conn {
		s.registrationPendingTCP, s.registrationPendingCancel, s.registrationPendingErr = nil, nil, nil
	}
	s.mu.Unlock()
	pending.cancelSM7()
	closeErr := pending.conn.Close()
	err := pending.switcher.AbortAddedIPSec3GPP()
	logging.Info("IMS refresh SA switch aborted; keeping the active SAs",
		"device", s.DeviceID(), "cseq", session.cseq, "cause", errorString(cause),
		"sm7_local", pending.conn.LocalAddr(), "elapsed_ms", time.Since(pending.started).Milliseconds(),
		"rollback_err", errorString(errors.Join(err, closeErr)))
}
