package imscore

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func largeRegisterTestConfig(registrar string) *IMSConfig {
	cfg := registerTransportTestConfig("auto", registrar)
	cfg.UserAgent = strings.Repeat("x", maxUDPRequestBytes)
	return cfg
}

func TestAutoRegisterOverUDPLimitUsesTCP(t *testing.T) {
	tcpRegistrar, udpRegistrar := listenRegisterTransports(t)
	defer tcpRegistrar.Close()
	defer udpRegistrar.Close()
	seen := make(chan string, 1)
	result := make(chan error, 1)
	go serveTCPRegisterStatus(tcpRegistrar, seen, result)

	svc, err := New(largeRegisterTestConfig(tcpRegistrar.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.StopCurrent()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := svc.Register(ctx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	request := <-seen
	if len(request) <= maxUDPRequestBytes || !strings.HasPrefix(sipHeaderValue(request, "Via"), "SIP/2.0/TCP ") ||
		!strings.Contains(sipHeaderValue(request, "Contact"), ";transport=tcp>") {
		t.Fatalf("large auto REGISTER was not sent as TCP (%d bytes): %q", len(request), sipHeaderValue(request, "Via"))
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	assertNoUDPRegister(t, udpRegistrar)
}

func TestAutoRegisterOverUDPLimitStaysOnUDPWhenTCPIsRefused(t *testing.T) {
	registrar, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer registrar.Close()
	seen := make(chan string, 1)
	go serveRegisterStatus(registrar, 200, seen)

	svc, err := New(largeRegisterTestConfig(registrar.LocalAddr().String()))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.StopCurrent()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := svc.Register(ctx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if request := <-seen; !strings.HasPrefix(sipHeaderValue(request, "Via"), "SIP/2.0/UDP ") {
		t.Fatalf("REGISTER did not fall back to UDP: %q", sipHeaderValue(request, "Via"))
	}
}
