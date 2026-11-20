package api

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/yibaiba/hideck/pkg/logger"
)

const (
	defaultHTTPSAddress     = ":7576"
	maxServerHeaderBytes    = 1 << 20
	managedHTTPWriteTimeout = 120 * time.Second
)

func (s *Server) serveHTTP() error {
	handler := withCommandEventStreamDeadlineDisabled(s.newRouter())
	httpServer := newManagedHTTPServer(s.cfg.Port, handler)
	httpListener, err := net.Listen("tcp", httpServer.Addr)
	if err != nil {
		return fmt.Errorf("监听 HTTP %s: %w", httpServer.Addr, err)
	}
	httpServer.Addr = httpListener.Addr().String()
	s.httpSrvMu.Lock()
	s.httpSrv, s.httpsSrv = httpServer, nil
	s.httpSrvMu.Unlock()
	logger.Info("启动 HTTP 管理服务器", "address", httpListener.Addr().String())
	return serverFailure(httpServer.Serve(httpListener))
}

func (s *Server) serveHTTPAndHTTPS(certificate tls.Certificate) error {
	handler := withCommandEventStreamDeadlineDisabled(s.newRouter())
	httpServer := newManagedHTTPServer(s.cfg.Port, handler)
	httpsAddress := s.cfg.HTTPSPort
	if httpsAddress == "" {
		httpsAddress = defaultHTTPSAddress
	}
	httpsServer := newManagedHTTPServer(httpsAddress, handler)
	httpListener, err := net.Listen("tcp", httpServer.Addr)
	if err != nil {
		return fmt.Errorf("监听 HTTP %s: %w", httpServer.Addr, err)
	}
	httpsListener, err := net.Listen("tcp", httpsServer.Addr)
	if err != nil {
		return errors.Join(fmt.Errorf("监听 HTTPS %s: %w", httpsServer.Addr, err), httpListener.Close())
	}
	tlsListener := tls.NewListener(httpsListener, &tls.Config{
		Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12,
	})
	httpServer.Addr = httpListener.Addr().String()
	httpsServer.Addr = httpsListener.Addr().String()
	s.httpSrvMu.Lock()
	s.httpSrv, s.httpsSrv = httpServer, httpsServer
	s.httpSrvMu.Unlock()
	logger.Info("启动 HTTP 管理服务器", "address", httpListener.Addr().String())
	logger.Info("启动 HTTPS 电话服务器", "address", httpsListener.Addr().String())
	return serveServerPair(httpServer, httpListener, httpsServer, tlsListener)
}

func newManagedHTTPServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 120 * time.Second, WriteTimeout: managedHTTPWriteTimeout,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: maxServerHeaderBytes,
	}
}

func serveServerPair(
	httpServer *http.Server,
	httpListener net.Listener,
	httpsServer *http.Server,
	httpsListener net.Listener,
) error {
	errorsCh := make(chan error, 2)
	go func() { errorsCh <- httpServer.Serve(httpListener) }()
	go func() { errorsCh <- httpsServer.Serve(httpsListener) }()
	first := <-errorsCh
	_ = httpServer.Close()
	_ = httpsServer.Close()
	second := <-errorsCh
	return errors.Join(serverFailure(first), serverFailure(second))
}

func serverFailure(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
