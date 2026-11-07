package imscore

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/iniwex5/vowifi-go/internal/vowifi/logging"
)

const (
	portSClosePeerReset = "peer_reset"
	portSCloseEOF       = "eof"
	portSCloseLocal     = "local_close"
	portSCloseOther     = "other"
	portSCloseTimeout   = "transport_timeout"
	portSCloseDeadline  = "read_deadline"
	// Closed by the UE when a refresh switch deleted the SAs under it; the
	// P-CSCF reopens port-s over the new SAs (TS 33.203 7.4.1a).
	portSCloseSARetired = "sa_retired"
)

type portSConnectionState struct {
	generation   uint64
	openedAt     time.Time
	registrar    string
	localClosing bool
	saRetired    bool
}

type portSSessionState struct {
	connections     map[net.Conn]portSConnectionState
	generation      uint64
	openedAt        time.Time
	closedAt        time.Time
	lastInboundAt   time.Time
	lastCloseKind   string
	lastCloseReason string
	closedRegistrar string
	peerResetCount  uint64
	resetRecovery   portSResetRecoveryState
	timeoutRecovery portSTimeoutRecoveryState
}

type portSSessionSnapshot struct {
	connected       bool
	generation      uint64
	openedAt        time.Time
	closedAt        time.Time
	lastInboundAt   time.Time
	lastCloseKind   string
	lastCloseReason string
	closedRegistrar string
	peerResetCount  uint64
	// REGISTER after a timeout did not bring port-s back where the carrier
	// requires it (Vodafone UK); the downlink is known to be broken.
	timeoutValidationFailed bool
}

func (s *Service) recordPortSOpened(conn net.Conn, now time.Time) {
	if s == nil || conn == nil {
		return
	}
	registrar := s.currentPortSRecoveryRegistrar()
	s.portSSessionMu.Lock()
	generation := s.recordPortSOpenedLocked(conn, now, registrar)
	s.portSSessionMu.Unlock()
	s.logPortSOpened(conn, registrar, generation)
}

// The caller owns portSSessionMu.
func (s *Service) recordPortSOpenedLocked(conn net.Conn, now time.Time, registrar string) uint64 {
	if s.portSSession.connections == nil {
		s.portSSession.connections = make(map[net.Conn]portSConnectionState)
	}
	s.portSSession.generation++
	s.portSSession.connections[conn] = portSConnectionState{
		generation: s.portSSession.generation, openedAt: now, registrar: registrar,
	}
	s.portSSession.openedAt = now
	return s.portSSession.generation
}

func (s *Service) logPortSOpened(conn net.Conn, registrar string, generation uint64) {
	logging.Info("IMS port-s lifecycle",
		"device", s.DeviceID(), "event", "opened", "generation", generation,
		"pcscf", registrar, "inner_ip", s.cfg.LocalAddr,
		"remote", conn.RemoteAddr(), "local", conn.LocalAddr())
}

func (s *Service) recordPortSInbound(now time.Time) {
	if s == nil {
		return
	}
	s.portSSessionMu.Lock()
	s.portSSession.lastInboundAt = now
	s.portSSessionMu.Unlock()
}

// recordPortSClosed logs every close, but only a relevant non-local close may
// drive recovery. A detached reader may finish after its replacement failed.
func (s *Service) recordPortSClosed(conn net.Conn, err error, now time.Time) bool {
	if s == nil || conn == nil {
		return false
	}
	// Order close evidence against accepted UDP requests and failover commits.
	var probe *portSTimeoutProbeEpisode
	s.mu.Lock()
	currentRegistrar := strings.TrimSpace(s.registrar)
	s.portSSessionMu.Lock()
	connection, tracked := s.portSSession.connections[conn]
	delete(s.portSSession.connections, conn)
	registrar := connection.registrar
	if registrar == "" {
		registrar = currentRegistrar
	}
	kind := classifyPortSClose(err)
	if connection.localClosing {
		kind = portSCloseLocal
	}
	if connection.saRetired {
		kind = portSCloseSARetired
	}
	// Retired cleanup must not replace the latest failure. An older live
	// connection still matters if it is the final downlink on this registrar,
	// unless it went with the old SAs after a newer flow had opened.
	relevant := tracked && strings.EqualFold(registrar, currentRegistrar)
	current := relevant &&
		(connection.generation == s.portSSession.generation ||
			(kind != portSCloseLocal && kind != portSCloseSARetired &&
				!s.hasLivePortSConnectionLocked(currentRegistrar)))
	if current {
		if kind != portSCloseLocal {
			s.udpDownlinkProven.Store(false)
			s.invalidateSMSReceptionLocked()
		}
		s.portSSession.closedAt = now
		s.portSSession.lastCloseKind = kind
		s.portSSession.lastCloseReason = errorText(err)
		s.portSSession.closedRegistrar = registrar
		s.recordPortSTimeoutLocked(registrar, kind)
		if kind == portSCloseTimeout && s.timeoutAwaitPreset() && !s.sipOutboundFlow.Load() {
			probe = &portSTimeoutProbeEpisode{generation: s.portSSession.generation, registrar: currentRegistrar}
			s.portSTimeoutProbe.Store(probe)
		}
	}
	startFailover := false
	if current && kind == portSClosePeerReset {
		s.portSSession.peerResetCount++
		startFailover = s.armVodafoneUKResetRecoveryLocked(registrar, connection.openedAt, now)
	}
	generation := connection.generation
	lastInboundAt := s.portSSession.lastInboundAt
	s.portSSessionMu.Unlock()
	s.mu.Unlock()
	logging.Info("IMS port-s lifecycle",
		"device", s.DeviceID(), "event", "closed", "generation", generation,
		"pcscf", registrar, "inner_ip", s.cfg.LocalAddr,
		"close_kind", kind, "reason", errorText(err),
		"lifetime", elapsedSince(connection.openedAt, now),
		"last_inbound_at", lastInboundAt)
	if probe != nil {
		logging.Info("IMS port-s timed out; SMS reception stays ready until the probe REGISTER answers",
			"device", s.DeviceID(), "pcscf", probe.registrar, "port_s_generation", probe.generation,
			"carrier_preset", strings.TrimSpace(s.cfg.CarrierPresetID))
	}
	if startFailover {
		s.startPendingPortSResetFailover()
	}
	// Reader cleanup can interleave after recording closes. Every relevant
	// non-local close must check the actual live connections after untracking,
	// even if a newer close already supplied the lifecycle diagnostic.
	return relevant && kind != portSCloseLocal
}

func (s *Service) hasLivePortSConnectionLocked(registrar string) bool {
	for _, connection := range s.portSSession.connections {
		if !connection.localClosing && strings.EqualFold(connection.registrar, registrar) {
			return true
		}
	}
	return false
}

// The caller owns protectedConnMu. Intersect both registries because a closed
// reader is removed from the port-s session before protected cleanup finishes.
func (s *Service) hasCurrentTrackedPortSLocked(registrar string) bool {
	s.portSSessionMu.Lock()
	defer s.portSSessionMu.Unlock()
	for conn := range s.protectedConns {
		state, tracked := s.portSSession.connections[conn]
		if tracked && !state.localClosing && strings.EqualFold(state.registrar, registrar) {
			return true
		}
	}
	return false
}

func (s *Service) markPortSLocalClose(conn net.Conn) {
	if s == nil || conn == nil {
		return
	}
	s.portSSessionMu.Lock()
	state, exists := s.portSSession.connections[conn]
	if exists {
		state.localClosing = true
		s.portSSession.connections[conn] = state
	}
	s.portSSessionMu.Unlock()
}

// markPortSSARetired classifies the coming close of a port-s flow whose SAs a
// refresh switch deleted.
func (s *Service) markPortSSARetired(conn net.Conn) {
	if s == nil || conn == nil {
		return
	}
	s.portSSessionMu.Lock()
	state, exists := s.portSSession.connections[conn]
	if exists {
		state.saRetired = true
		s.portSSession.connections[conn] = state
	}
	s.portSSessionMu.Unlock()
}

func (s *Service) portSRegistrar(conn net.Conn) string {
	if s == nil || conn == nil {
		return ""
	}
	s.portSSessionMu.Lock()
	registrar := s.portSSession.connections[conn].registrar
	s.portSSessionMu.Unlock()
	return strings.TrimSpace(registrar)
}

func (s *Service) capturePortSSession() portSSessionSnapshot {
	if s == nil {
		return portSSessionSnapshot{}
	}
	s.portSSessionMu.Lock()
	defer s.portSSessionMu.Unlock()
	state := s.portSSession
	return portSSessionSnapshot{
		connected: s.portSPushReady.Load(), generation: state.generation,
		openedAt: state.openedAt, closedAt: state.closedAt,
		lastInboundAt: state.lastInboundAt, lastCloseKind: state.lastCloseKind,
		lastCloseReason: state.lastCloseReason, closedRegistrar: state.closedRegistrar,
		peerResetCount:          state.peerResetCount,
		timeoutValidationFailed: state.timeoutRecovery.validationFailed,
	}
}

func classifyPortSClose(err error) string {
	if err == nil {
		return portSCloseOther
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) {
		return portSCloseLocal
	}
	if errors.Is(err, syscall.ECONNRESET) || strings.Contains(strings.ToLower(err.Error()), "connection reset by peer") {
		return portSClosePeerReset
	}
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return portSCloseDeadline
	}
	var timeout net.Error
	if errors.Is(err, syscall.ETIMEDOUT) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return portSCloseTimeout
	}
	if errors.Is(err, io.EOF) {
		return portSCloseEOF
	}
	return portSCloseOther
}

func elapsedSince(start, end time.Time) time.Duration {
	if start.IsZero() || end.Before(start) {
		return 0
	}
	return end.Sub(start).Round(time.Millisecond)
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Service) recordIMSRegistrationSucceeded() {
	if s != nil {
		s.registrationGeneration.Add(1)
	}
}

func (s *Service) logIMSRegistrationAttempt(session *registerSession, request string) {
	if s == nil || session == nil {
		return
	}
	portS := s.capturePortSSession()
	regID, _ := contactParameterValue(rawSIPHeaderValue(request, "Contact"), "reg-id")
	logging.Info("IMS REGISTER attempt",
		"device", s.DeviceID(), "cseq", session.cseq,
		"pcscf", s.currentPortSRecoveryRegistrar(), "inner_ip", s.cfg.LocalAddr,
		"reg_id", strings.TrimSpace(regID), "port_s_generation", portS.generation,
		"port_s_connected", portS.connected, "port_s_last_inbound_at", portS.lastInboundAt)
}

func (s *Service) registeredFlowRegID() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return registeredFlowRegIDLocked(s)
}

func registeredFlowRegIDLocked(s *Service) int {
	if s != nil && s.outboundContactRegistered {
		return 1
	}
	return 0
}
