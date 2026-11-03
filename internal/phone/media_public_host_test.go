package phone

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewMediaManagerReportsPublicHostResolutionFailure(t *testing.T) {
	wantErr := errors.New("lookup failed")
	_, err := NewMediaManager(MediaOptions{
		UDPAddress: ":0", PublicHost: "hideck.example.com",
		lookupPublicIP: func(context.Context, string) ([]net.IPAddr, error) { return nil, wantErr },
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("NewMediaManager() error = %v", err)
	}
}

// hideck#93: the public host is looked up again for every call, and a failed
// lookup keeps the last good address.
func TestMediaManagerFollowsPublicHostAddressChanges(t *testing.T) {
	var mu sync.Mutex
	current, lookupErr := "203.0.113.10", error(nil)
	setAnswer := func(ip string, err error) {
		mu.Lock()
		defer mu.Unlock()
		current, lookupErr = ip, err
	}
	manager, err := NewMediaManager(MediaOptions{
		UDPAddress: ":0", PublicHost: "hideck.example.com",
		lookupPublicIP: func(context.Context, string) ([]net.IPAddr, error) {
			mu.Lock()
			defer mu.Unlock()
			if lookupErr != nil {
				return nil, lookupErr
			}
			return []net.IPAddr{{IP: net.ParseIP(current)}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	answerFor := func() string {
		t.Helper()
		browser, _, _ := newPCMUBrowserPeer(t)
		defer browser.Close()
		answer, err := manager.Create(context.Background(), "admin", completeLocalOffer(t, browser), "")
		if err != nil {
			t.Fatal(err)
		}
		manager.Remove(answer.MediaID)
		return answer.SDP
	}

	if sdp := answerFor(); !strings.Contains(sdp, " 203.0.113.10 ") {
		t.Fatalf("first call does not offer the startup address:\n%s", sdp)
	}
	setAnswer("198.51.100.20", nil)
	if sdp := answerFor(); !strings.Contains(sdp, " 198.51.100.20 ") || strings.Contains(sdp, " 203.0.113.10 ") {
		t.Fatalf("call after the address changed:\n%s", sdp)
	}
	setAnswer("", errors.New("temporary DNS failure"))
	if sdp := answerFor(); !strings.Contains(sdp, " 198.51.100.20 ") {
		t.Fatalf("call during a failed lookup does not keep the last address:\n%s", sdp)
	}
}

func TestWebRTCPublicHostDoesNotFallBackWhenCallSetupIsCancelled(t *testing.T) {
	hang := false
	host := newWebRTCPublicHost("hideck.example.com", func(ctx context.Context, _ string) ([]net.IPAddr, error) {
		if hang {
			// A resolver that never answers: only the context ends the lookup.
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.10")}}, nil
	})
	if _, err := host.resolve(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	hang = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ips, err := host.resolve(ctx, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("resolve(cancelled) = %v, %v; want context.Canceled", ips, err)
	}
	// The per-call lookup timeout expiring is a DNS failure: keep the last address.
	if ips, err := host.resolve(context.Background(), 10*time.Millisecond); err != nil || len(ips) != 1 || ips[0] != "203.0.113.10" {
		t.Fatalf("resolve(after timeout) = %v, %v", ips, err)
	}
}

func TestSameIPSetIgnoresOrder(t *testing.T) {
	if !sameIPSet([]string{"203.0.113.10", "2001:db8::10"}, []string{"2001:db8::10", "203.0.113.10"}) {
		t.Fatal("reordered round-robin answer counted as a change")
	}
	if sameIPSet([]string{"203.0.113.10"}, []string{"198.51.100.20"}) {
		t.Fatal("a new address was not counted as a change")
	}
}

func TestResolveWebRTCPublicIPs(t *testing.T) {
	lookupCalls := 0
	lookup := func(_ context.Context, host string) ([]net.IPAddr, error) {
		lookupCalls++
		if host != "hideck.example.com" {
			t.Fatalf("lookup host = %q", host)
		}
		return []net.IPAddr{
			{IP: net.ParseIP("203.0.113.10")},
			{IP: net.ParseIP("203.0.113.10")},
			{IP: net.ParseIP("2001:db8::10")},
		}, nil
	}

	got, err := resolveWebRTCPublicIPs(context.Background(), "hideck.example.com", lookup)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"203.0.113.10", "2001:db8::10"}
	if !reflect.DeepEqual(got, want) || lookupCalls != 1 {
		t.Fatalf("resolveWebRTCPublicIPs() = %v, calls = %d", got, lookupCalls)
	}

	got, err = resolveWebRTCPublicIPs(context.Background(), "203.0.113.20", lookup)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"203.0.113.20"}) || lookupCalls != 1 {
		t.Fatalf("IP literal result = %v, calls = %d", got, lookupCalls)
	}
}
