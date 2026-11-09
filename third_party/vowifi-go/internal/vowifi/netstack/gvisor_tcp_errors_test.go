package netstack

import (
	"errors"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

type failingTCPEndpoint struct {
	tcpip.Endpoint
	failure tcpip.Error
}

func (e *failingTCPEndpoint) Read(io.Writer, tcpip.ReadOptions) (tcpip.ReadResult, tcpip.Error) {
	return tcpip.ReadResult{}, e.failure
}

func (e *failingTCPEndpoint) Write(tcpip.Payloader, tcpip.WriteOptions) (int64, tcpip.Error) {
	return 0, e.failure
}

func TestIMSTCPConnPreservesGonetFailureKinds(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure tcpip.Error
		want    error
	}{
		{"transport timeout", &tcpip.ErrTimeout{}, syscall.ETIMEDOUT},
		{"peer reset", &tcpip.ErrConnectionReset{}, syscall.ECONNRESET},
		{"local deadline", &tcpip.ErrTimeout{}, os.ErrDeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn := newFailingIMSTCPConn(t, test.failure)
			if test.want == os.ErrDeadlineExceeded {
				if err := conn.SetDeadline(time.Now().Add(-time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			for _, operation := range []func([]byte) (int, error){conn.Read, conn.Write} {
				_, err := operation(make([]byte, 1))
				if !errors.Is(err, test.want) {
					t.Fatalf("gonet adapter error = %v, want %v", err, test.want)
				}
				if test.want == os.ErrDeadlineExceeded && errors.Is(err, syscall.ETIMEDOUT) {
					t.Fatal("local deadline was exposed as a transport failure")
				}
			}
		})
	}
}

func newFailingIMSTCPConn(t *testing.T, failure tcpip.Error) *imsTCPConn {
	t.Helper()
	networkStack := newStack()
	t.Cleanup(networkStack.Destroy)
	queue := &waiter.Queue{}
	endpoint, err := networkStack.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, queue)
	if err != nil {
		t.Fatalf("NewEndpoint: %s", err)
	}
	conn := &imsTCPConn{TCPConn: gonet.NewTCPConn(queue, &failingTCPEndpoint{Endpoint: endpoint, failure: failure})}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestNormalizeGonetTCPErrorPreservesOtherErrorsAndMetadata(t *testing.T) {
	for _, err := range []error{nil, io.EOF, net.ErrClosed, errors.New("unrelated operation timed out")} {
		if got := normalizeGonetTCPError(err); got != err {
			t.Fatalf("unrelated error changed: %v -> %v", err, got)
		}
	}
	original := &net.OpError{
		Op: "read", Net: "tcp", Source: &net.TCPAddr{Port: 1234}, Addr: &net.TCPAddr{Port: 5060},
		Err: errors.New((&tcpip.ErrTimeout{}).String()),
	}
	normalized := normalizeGonetTCPError(original).(*net.OpError)
	if normalized == original || errors.Is(original, syscall.ETIMEDOUT) {
		t.Fatal("normalization mutated the source error")
	}
	if normalized.Op != original.Op || normalized.Net != original.Net ||
		normalized.Source != original.Source || normalized.Addr != original.Addr {
		t.Fatalf("operation metadata lost: %+v", normalized)
	}
}
