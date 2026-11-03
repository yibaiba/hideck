package phone

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMediaManagerReportsInitialPublicHostResolutionFailure(t *testing.T) {
	wantErr := errors.New("lookup failed")
	_, err := NewMediaManager(MediaOptions{
		UDPAddress: ":0", PublicHost: "hideck.example.com",
		LookupIPAddr: func(context.Context, string) ([]net.IPAddr, error) {
			return nil, wantErr
		},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("NewMediaManager() error = %v", err)
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
	got, err = resolveWebRTCPublicIPs(context.Background(), "", lookup)
	if err != nil || len(got) != 0 || lookupCalls != 1 {
		t.Fatalf("empty host result = %v, err = %v, calls = %d", got, err, lookupCalls)
	}
}

func TestMediaManagerRefreshesPublicHostForEachSession(t *testing.T) {
	publicIP := "203.0.113.10"
	manager, err := NewMediaManager(MediaOptions{
		UDPAddress: ":0", PublicHost: " hideck.example.com ",
		LookupIPAddr: func(_ context.Context, host string) ([]net.IPAddr, error) {
			if host != "hideck.example.com" {
				t.Fatalf("lookup host = %q", host)
			}
			return []net.IPAddr{{IP: net.ParseIP(publicIP)}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	browser, _, _ := newPCMUBrowserPeer(t)
	t.Cleanup(func() { _ = browser.Close() })
	offer := completeLocalOffer(t, browser)
	ctx, cancel := context.WithTimeout(context.Background(), mediaIntegrationTimeout)
	defer cancel()
	var firstSession *MediaSession
	for index, ip := range []string{"203.0.113.10", "203.0.113.20", "203.0.113.30"} {
		previousIP := publicIP
		publicIP = ip
		answer, err := manager.Create(ctx, "admin", offer, "")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(answer.SDP, " "+ip+" ") {
			t.Fatalf("session %d SDP is missing current IP %s:\n%s", index, ip, answer.SDP)
		}
		if index == 0 {
			firstSession = manager.Get(answer.MediaID)
			continue
		}
		if strings.Contains(answer.SDP, " "+previousIP+" ") {
			t.Fatalf("session %d SDP retains previous IP %s:\n%s", index, previousIP, answer.SDP)
		}
	}
	if sdp := firstSession.peer.LocalDescription().SDP; !strings.Contains(sdp, " 203.0.113.10 ") {
		t.Fatalf("existing session lost its original public IP:\n%s", sdp)
	}
}

func TestMediaManagerReportsPublicHostRefreshFailures(t *testing.T) {
	for _, wantErr := range []error{errors.New("DNS unavailable"), context.Canceled} {
		t.Run(wantErr.Error(), func(t *testing.T) {
			testMediaManagerPublicHostRefreshFailure(t, wantErr)
		})
	}
}

func testMediaManagerPublicHostRefreshFailure(t *testing.T, wantErr error) {
	t.Helper()
	var lookupErr error
	manager, err := NewMediaManager(MediaOptions{
		UDPAddress: ":0", PublicHost: "hideck.example.com",
		LookupIPAddr: func(ctx context.Context, _ string) ([]net.IPAddr, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return []net.IPAddr{{IP: net.ParseIP("203.0.113.10")}}, lookupErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if errors.Is(wantErr, context.Canceled) {
		cancel()
	} else {
		lookupErr = wantErr
	}
	answer, err := manager.Create(ctx, "admin", "unused offer", "")
	if !errors.Is(err, wantErr) {
		t.Fatalf("Create() error = %v, want %v", err, wantErr)
	}
	if answer != (MediaAnswer{}) || len(manager.sessions) != 0 {
		t.Fatalf("failed DNS refresh created media: %+v", answer)
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

func TestWebRTCPublicHostHonorsCallerDeadline(t *testing.T) {
	blockLookup := false
	host := newWebRTCPublicHost("hideck.example.com", func(ctx context.Context, _ string) ([]net.IPAddr, error) {
		if blockLookup {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.10")}}, nil
	})
	if _, err := host.resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	blockLookup = true
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	ips, err := host.resolve(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || len(ips) != 0 {
		t.Fatalf("resolve() = %v, %v; want no addresses and context.DeadlineExceeded", ips, err)
	}
}
