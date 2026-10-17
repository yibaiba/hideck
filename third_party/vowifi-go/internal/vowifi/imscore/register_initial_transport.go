package imscore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	vowifidns "github.com/iniwex5/vowifi-go/internal/vowifi/dns"
	"github.com/iniwex5/vowifi-go/internal/vowifi/logging"
	"github.com/iniwex5/vowifi-go/internal/vowifi/sipkit"
)

type initialRegistrationTransport struct {
	kind   string
	remote *net.UDPAddr
	packet net.PacketConn
	stream net.Conn
	port   int
}

func registerTransportCandidates(configured string) []string {
	switch strings.ToLower(strings.TrimSpace(configured)) {
	case "tcp":
		return []string{"tcp", "udp"}
	case "udp":
		return []string{"udp"}
	default:
		return []string{"udp", "tcp"}
	}
}

func (s *Service) openInitialRegistrationTransport(
	ctx context.Context,
	serverListener, clientReservation net.Listener,
) error {
	candidates := registerTransportCandidates(s.cfg.Transport)
	var failures []error
	for _, candidate := range candidates {
		opened, openErr := s.openRegisterCandidate(ctx, candidate)
		if openErr != nil {
			failures = append(failures, openErr)
			continue
		}
		s.activateInitialRegistrationTransport(opened, serverListener, clientReservation)
		return nil
	}
	return wrapRegisterTransportFailures(failures)
}

// maxUDPRequestBytes is the RFC 3261 18.1.1 limit for a request when the path
// MTU is unknown. Above it the request must use a congestion-controlled
// transport such as TCP.
const maxUDPRequestBytes = 1300

const largeRegisterTCPDialTimeout = 5 * time.Second

// moveLargeRegisterToTCP switches an auto-transport REGISTER that is still on
// unprotected UDP to TCP when the request is over the RFC 3261 limit. A UDP
// REGISTER that size is fragmented, and Globe PH drops the fragments without
// an answer or ICMP. If TCP cannot be opened the request stays on UDP, as
// 18.1.1 allows after a refused connection.
func (s *Service) moveLargeRegisterToTCP(ctx context.Context, session *registerSession, size int) bool {
	if size <= maxUDPRequestBytes || !isAutoRegisterTransport(s.cfg.Transport) ||
		registerUsesProtectedTransport(session) || s.usesExternalRegistrationTransport() ||
		s.currentRegistrationTransport() != "udp" || s.largeRegisterTCPFailed.Load() {
		return false
	}
	candidates := registerTransportCandidates(s.cfg.Transport)
	// A dropped SYN must not use up the time the UDP attempt needs.
	dialTimeout := largeRegisterTCPDialTimeout
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline)/2 < dialTimeout {
		dialTimeout = time.Until(deadline) / 2
	}
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	if _, err := s.replaceInitialRegistrationTransport(dialCtx, candidates, indexOfRegisterTransport(candidates, "tcp")); err != nil {
		logging.Info("IMS REGISTER exceeds the UDP size limit but TCP is unavailable; sending over UDP",
			"device", s.DeviceID(), "cseq", session.cseq, "bytes", size, "limit", maxUDPRequestBytes,
			"dial_timeout", dialTimeout, "err", err)
		return false
	}
	logging.Info("IMS REGISTER exceeds the UDP size limit; sending over TCP",
		"device", s.DeviceID(), "cseq", session.cseq, "bytes", size, "limit", maxUDPRequestBytes)
	s.largeRegisterMovedToTCP.Store(true)
	return true
}

func isAutoRegisterTransport(configured string) bool {
	value := strings.ToLower(strings.TrimSpace(configured))
	return value == "" || value == "auto"
}

func (s *Service) openRegisterCandidate(ctx context.Context, transport string) (*initialRegistrationTransport, error) {
	remote, err := s.resolveRegistrar(ctx, transport)
	if err != nil {
		return nil, fmt.Errorf("%s registrar: %w", transport, err)
	}
	if transport == "tcp" {
		local := &net.TCPAddr{IP: s.cfg.LocalIP}
		conn, dialErr := s.cfg.IMSNetwork.DialTCPContext(ctx, local, udpToTCPAddr(remote))
		if dialErr != nil {
			return nil, fmt.Errorf("tcp connect: %w", dialErr)
		}
		return &initialRegistrationTransport{
			kind: transport, remote: remote, stream: conn, port: tcpPort(conn.LocalAddr()),
		}, nil
	}
	conn, err := s.cfg.IMSNetwork.ListenPacket("udp", &net.UDPAddr{IP: s.cfg.LocalIP})
	if err != nil {
		return nil, fmt.Errorf("udp listen: %w", err)
	}
	port := 0
	if address, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		port = address.Port
	}
	return &initialRegistrationTransport{kind: transport, remote: remote, packet: conn, port: port}, nil
}

func udpToTCPAddr(address *net.UDPAddr) *net.TCPAddr {
	return &net.TCPAddr{IP: append(net.IP(nil), address.IP...), Port: address.Port, Zone: address.Zone}
}

func (s *Service) activateInitialRegistrationTransport(
	opened *initialRegistrationTransport,
	serverListener, clientReservation net.Listener,
) {
	if opened.packet != nil {
		opened.packet = &inboundCountingPacketConn{conn: opened.packet, service: s}
	}
	if opened.stream != nil {
		s.configureRegistrationTCPKeepalive(opened.stream)
		opened.stream = s.newInboundCountingConn(opened.stream)
	}
	s.cfg.LocalPort = opened.port
	s.mu.Lock()
	s.registrationIO = opened.packet
	s.registrationTCP = opened.stream
	s.registrationTCPProtected = false
	s.registrationTransport = opened.kind
	s.registrationRemote = cloneUDPAddr(opened.remote)
	// A transport fallback passes nil reservations and must keep the sec-agree ports alive.
	if serverListener != nil {
		s.securityServerIO = serverListener
		s.protectedServerPort = tcpPort(serverListener.Addr())
	}
	if clientReservation != nil {
		s.clientPortReserve = clientReservation
		s.protectedClientPort = tcpPort(clientReservation.Addr())
	}
	s.mu.Unlock()
	s.activateInitialSendAndReceive(opened)
	if serverListener != nil {
		s.networkDone.Add(1)
		go s.acceptProtectedSIP(serverListener)
	}
}

func (s *Service) activateInitialSendAndReceive(opened *initialRegistrationTransport) {
	if opened.stream != nil {
		s.configureRegistrationTCPKeepalive(opened.stream)
		s.transport.SetSendFn(func(request string) error {
			return s.writeSIPStream(opened.stream, request)
		})
		s.networkDone.Add(1)
		go s.readRegistrationStream(opened.stream)
		return
	}
	s.transport.SetSendFn(func(request string) error {
		remote := s.currentRegistrationRemote()
		if remote == nil {
			return errors.New("imscore: registrar address is unavailable")
		}
		if _, err := opened.packet.WriteTo([]byte(request), remote); err != nil {
			return fmt.Errorf("imscore: send REGISTER datagram: %w", err)
		}
		return nil
	})
	s.networkDone.Add(1)
	go s.readRegistrationResponses(opened.packet)
}

func closeRegistrationReservations(serverListener, clientReservation net.Listener) {
	if serverListener != nil {
		_ = serverListener.Close()
	}
	if clientReservation != nil {
		_ = clientReservation.Close()
	}
}

func (s *Service) replaceInitialRegistrationTransport(
	ctx context.Context,
	candidates []string,
	start int,
) (int, error) {
	if start < 0 {
		start = 0
	}
	if start >= len(candidates) {
		return -1, errors.New("imscore: no further REGISTER transport")
	}
	var failures []error
	for index := start; index < len(candidates); index++ {
		opened, err := s.openRegisterCandidate(ctx, candidates[index])
		if err != nil {
			failures = append(failures, err)
			continue
		}
		s.closeInitialRegistrationTransport()
		s.activateInitialRegistrationTransport(opened, nil, nil)
		return index, nil
	}
	return -1, wrapRegisterTransportFailures(failures)
}

func wrapRegisterTransportFailures(failures []error) error {
	if joined := errors.Join(failures...); joined != nil {
		return fmt.Errorf("imscore: open REGISTER transport: %w", joined)
	}
	return errors.New("imscore: no further REGISTER transport")
}

func (s *Service) closeInitialRegistrationTransport() {
	s.mu.Lock()
	packet := s.registrationIO
	stream := s.registrationTCP
	s.registrationIO = nil
	s.registrationTCP = nil
	s.registrationTCPProtected = false
	s.registrationTransport = ""
	s.registrationRemote = nil
	s.mu.Unlock()
	if packet != nil {
		_ = packet.Close()
	}
	if stream != nil {
		_ = stream.Close()
	}
}

func (s *Service) resetRegistrationTransportForRegistrarRetry() {
	s.resetPortSRecoveryKnowledge()
	s.closeProtectedUDP()
	s.closeInitialRegistrationTransport()
	s.mu.Lock()
	server := s.securityServerIO
	reservation := s.clientPortReserve
	s.securityServerIO = nil
	s.clientPortReserve = nil
	s.protectedClientPort = 0
	s.protectedServerPort = 0
	s.mu.Unlock()
	closeRegistrationReservations(server, reservation)
	s.transport.SetSendFn(nil)
}

func (s *Service) resolveRegistrar(ctx context.Context, transport string) (*net.UDPAddr, error) {
	target, err := s.selectRegistrarCandidate(ctx, transport)
	if err != nil {
		return nil, err
	}
	host, port, err := sipkit.ParseHostPortWithDefault(target, defaultSIPPort)
	if err != nil {
		return nil, fmt.Errorf("imscore: parse registrar %q: %w", target, err)
	}
	ip, err := s.cfg.IMSNetwork.ResolveIP(ctx, strings.Trim(host, "[]"))
	if err != nil {
		return nil, fmt.Errorf("imscore: resolve registrar %s: %w", host, err)
	}
	return &net.UDPAddr{IP: ip, Port: port}, nil
}

func (s *Service) discoverRegistrar(ctx context.Context, transport string) (string, error) {
	resolver, ok := s.cfg.IMSNetwork.(vowifidns.RegistrarNetwork)
	if !ok {
		return "", errors.New("imscore: IMS network does not support SRV lookup")
	}
	return vowifidns.DiscoverRegistrarViaNetwork(ctx, s.cfg.Domain, transport, resolver)
}
