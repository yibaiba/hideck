package phone

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"

	"github.com/yibaiba/hideck/pkg/logger"
)

type publicIPLookup func(context.Context, string) ([]net.IPAddr, error)

type webRTCPublicHost struct {
	host   string
	lookup publicIPLookup
	mu     sync.Mutex
	last   []string // Only used to log address changes, never as a DNS fallback.
}

func newWebRTCPublicHost(value string, lookup publicIPLookup) *webRTCPublicHost {
	host := strings.TrimSpace(value)
	if host == "" {
		return nil
	}
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIPAddr
	}
	return &webRTCPublicHost{host: host, lookup: lookup}
}

func (h *webRTCPublicHost) resolve(ctx context.Context) ([]string, error) {
	publicIPs, err := resolveWebRTCPublicIPs(ctx, h.host, h.lookup)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.last == nil {
		logger.Info("WebRTC 公网地址", "host", h.host, "ips", publicIPs)
	} else if !sameIPSet(h.last, publicIPs) {
		logger.Info("WebRTC 公网地址已变化", "host", h.host, "old", h.last, "new", publicIPs)
	}
	h.last = publicIPs
	return publicIPs, nil
}

// sameIPSet ignores order, which round-robin DNS changes between lookups.
func sameIPSet(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

func resolveWebRTCPublicIPs(ctx context.Context, host string, lookup publicIPLookup) ([]string, error) {
	if host == "" {
		return nil, nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return []string{ip.String()}, nil
	}
	addresses, err := lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("phone: resolve WebRTC public host %q: %w", host, err)
	}
	unique := make(map[string]struct{}, len(addresses))
	result := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if address.IP == nil {
			continue
		}
		ip := address.IP.String()
		if _, exists := unique[ip]; exists {
			continue
		}
		unique[ip] = struct{}{}
		result = append(result, ip)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("phone: WebRTC public host %q resolved without IP addresses", host)
	}
	return result, nil
}
