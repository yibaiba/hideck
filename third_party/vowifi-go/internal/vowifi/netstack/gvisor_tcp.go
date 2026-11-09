package netstack

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	imsTCPMSS        = 1100
	imsIPv4LinkMTU   = imsTCPMSS + header.IPv4MinimumSize + header.TCPMinimumSize
	imsIPv6LinkMTU   = header.IPv6MinimumMTU
	tcpListenBacklog = 4096
	// Port-s has no SIP CRLF. Probe after 30s of silence, then retry quickly
	// enough to expose a dead reverse flow before the SMSC attempts delivery.
	imsTCPKeepaliveIdle     = 30 * time.Second
	imsTCPKeepaliveInterval = 5 * time.Second
	imsTCPKeepaliveProbes   = 3
)

type tcpDialConfig struct {
	local    tcpip.FullAddress
	remote   tcpip.FullAddress
	protocol tcpip.NetworkProtocolNumber
	mss      int
}

func imsLinkMTU(protocol tcpip.NetworkProtocolNumber) uint32 {
	if protocol == ipv4.ProtocolNumber {
		return imsIPv4LinkMTU
	}
	return imsIPv6LinkMTU
}

func dialTCPWithMSS(ctx context.Context, networkStack *stack.Stack, cfg tcpDialConfig) (net.Conn, error) {
	mss := cfg.mss
	if mss <= 0 {
		mss = imsTCPMSS
	}
	var queue waiter.Queue
	endpoint, err := networkStack.NewEndpoint(tcp.ProtocolNumber, cfg.protocol, &queue)
	if err != nil {
		return nil, errors.New(err.String())
	}
	if err := endpoint.SetSockOptInt(tcpip.MaxSegOption, mss); err != nil {
		endpoint.Close()
		return nil, fmt.Errorf("set IMS TCP MSS: %s", err)
	}
	if err := configureIMSTCPKeepalive(endpoint); err != nil {
		endpoint.Close()
		return nil, err
	}
	if cfg.local != (tcpip.FullAddress{}) {
		if err := endpoint.Bind(cfg.local); err != nil {
			endpoint.Close()
			return nil, fmt.Errorf("bind IMS TCP endpoint: %s", err)
		}
	}
	if err := connectTCPEndpoint(ctx, endpoint, &queue, cfg.remote); err != nil {
		endpoint.Close()
		return nil, err
	}
	return newIMSTCPConn(&queue, endpoint, cfg.remote), nil
}

func configureIMSTCPKeepalive(endpoint tcpip.Endpoint) error {
	endpoint.SocketOptions().SetKeepAlive(true)
	idle := tcpip.KeepaliveIdleOption(imsTCPKeepaliveIdle)
	if err := endpoint.SetSockOpt(&idle); err != nil {
		return fmt.Errorf("set IMS TCP keepalive idle: %s", err)
	}
	interval := tcpip.KeepaliveIntervalOption(imsTCPKeepaliveInterval)
	if err := endpoint.SetSockOpt(&interval); err != nil {
		return fmt.Errorf("set IMS TCP keepalive interval: %s", err)
	}
	if err := endpoint.SetSockOptInt(tcpip.KeepaliveCountOption, imsTCPKeepaliveProbes); err != nil {
		return fmt.Errorf("set IMS TCP keepalive probes: %s", err)
	}
	return nil
}

func connectTCPEndpoint(ctx context.Context, endpoint tcpip.Endpoint, queue *waiter.Queue, remote tcpip.FullAddress) error {
	entry, ready := waiter.NewChannelEntry(waiter.WritableEvents)
	queue.EventRegister(&entry)
	defer queue.EventUnregister(&entry)

	err := endpoint.Connect(remote)
	if _, pending := err.(*tcpip.ErrConnectStarted); pending {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ready:
			err = endpoint.LastError()
		}
	}
	if err == nil {
		return nil
	}
	return normalizeGonetTCPError(&net.OpError{Op: "connect", Net: "tcp", Addr: tcpAddress(remote), Err: errors.New(err.String())})
}

func listenTCPWithMSS(networkStack *stack.Stack, address tcpip.FullAddress, protocol tcpip.NetworkProtocolNumber) (net.Listener, error) {
	queue := new(waiter.Queue)
	endpoint, err := networkStack.NewEndpoint(tcp.ProtocolNumber, protocol, queue)
	if err != nil {
		return nil, errors.New(err.String())
	}
	if err := endpoint.SetSockOptInt(tcpip.MaxSegOption, imsTCPMSS); err != nil {
		endpoint.Close()
		return nil, fmt.Errorf("set IMS TCP listener MSS: %s", err)
	}
	if err := endpoint.Bind(address); err != nil {
		endpoint.Close()
		return nil, fmt.Errorf("bind IMS TCP listener: %s", err)
	}
	if err := endpoint.Listen(tcpListenBacklog); err != nil {
		endpoint.Close()
		return nil, fmt.Errorf("listen on IMS TCP endpoint: %s", err)
	}
	return newIMSTCPListener(endpoint, queue), nil
}

// imsTCPListener applies TCP keepalive on every accepted connection.
// gVisor does not copy SO_KEEPALIVE from the listen endpoint on Accept,
// so port-s MT flows would otherwise sit idle until a NAT or P-CSCF timeout.
type imsTCPListener struct {
	ep     tcpip.Endpoint
	wq     *waiter.Queue
	cancel chan struct{}
	once   sync.Once
}

func newIMSTCPListener(ep tcpip.Endpoint, wq *waiter.Queue) *imsTCPListener {
	return &imsTCPListener{ep: ep, wq: wq, cancel: make(chan struct{})}
}

func (l *imsTCPListener) Addr() net.Addr {
	if l == nil || l.ep == nil {
		return nil
	}
	address, err := l.ep.GetLocalAddress()
	if err != nil {
		return nil
	}
	return tcpAddress(address)
}

func (l *imsTCPListener) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() { close(l.cancel) })
	if l.ep != nil {
		l.ep.Close()
	}
	return nil
}

func (l *imsTCPListener) Accept() (net.Conn, error) {
	conn, _, err := l.acceptTCP()
	return conn, err
}

func (l *imsTCPListener) acceptTCP() (net.Conn, tcpip.Endpoint, error) {
	var peer tcpip.FullAddress
	n, wq, err := l.ep.Accept(&peer)
	if _, blocked := err.(*tcpip.ErrWouldBlock); blocked {
		waitEntry, notifyCh := waiter.NewChannelEntry(waiter.ReadableEvents)
		l.wq.EventRegister(&waitEntry)
		defer l.wq.EventUnregister(&waitEntry)
		for {
			n, wq, err = l.ep.Accept(&peer)
			if _, blocked = err.(*tcpip.ErrWouldBlock); !blocked {
				break
			}
			select {
			case <-l.cancel:
				return nil, nil, net.ErrClosed
			case <-notifyCh:
			}
		}
	}
	if err != nil {
		return nil, nil, &net.OpError{
			Op: "accept", Net: "tcp", Addr: l.Addr(), Err: errors.New(err.String()),
		}
	}
	if kaErr := configureIMSTCPKeepalive(n); kaErr != nil {
		n.Close()
		return nil, nil, kaErr
	}
	return newIMSTCPConn(wq, n, peer), n, nil
}

func tcpAddress(address tcpip.FullAddress) *net.TCPAddr {
	return &net.TCPAddr{IP: net.IP(address.Addr.AsSlice()), Port: int(address.Port)}
}
