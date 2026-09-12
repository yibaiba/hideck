package device

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yibaiba/hideck/internal/db"
	"github.com/yibaiba/hideck/internal/upstreamproxy"
)

func loadDeviceCountryTableFixture(t *testing.T) {
	t.Helper()
	cachePath := filepath.Join(t.TempDir(), "mcc-mnc-table.json")
	rows := `[{"mcc":"310","mnc":"260","iso":"us","country":"United States","country_code":"US","network":"T-Mobile"}]`
	if err := os.WriteFile(cachePath, []byte(rows), 0o644); err != nil {
		t.Fatalf("WriteFile() error=%v", err)
	}
	result := upstreamproxy.InitCountryTable(context.Background(), upstreamproxy.CountryTableOptions{CachePath: cachePath})
	if result.Err != nil {
		t.Fatalf("InitCountryTable() error=%v", result.Err)
	}
}

func openDeviceTestDB(t *testing.T) {
	t.Helper()
	if err := db.Init(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("db.Init() error=%v", err)
	}
}

func TestResolveVoWiFiCountryProxySelectsUSProxy(t *testing.T) {
	openDeviceTestDB(t)
	loadDeviceCountryTableFixture(t)
	if err := db.UpsertUpstreamProxy(db.UpstreamProxy{ID: "proxy-us", Addr: "127.0.0.1:1080", Enabled: true, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatalf("UpsertUpstreamProxy() error=%v", err)
	}
	if err := db.UpsertUpstreamProxyCountryRule(db.UpstreamProxyCountryRule{CountryCode: "US", UpstreamProxyID: "proxy-us", Enabled: true}); err != nil {
		t.Fatalf("UpsertUpstreamProxyCountryRule() error=%v", err)
	}
	got, err := resolveVoWiFiCountryProxy(voWiFiProxyResolveRequest{
		HomeMCC: "310", TraceID: "trace-1", DeviceID: "dev-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != "proxy-us" || got.Addr != "127.0.0.1:1080" || !got.Enabled {
		t.Fatalf("resolveVoWiFiCountryProxy()=%+v, want proxy-us", got)
	}
}

func TestResolveVoWiFiCountryProxyDirectWhenNoCountryRule(t *testing.T) {
	openDeviceTestDB(t)
	loadDeviceCountryTableFixture(t)
	got, err := resolveVoWiFiCountryProxy(voWiFiProxyResolveRequest{
		HomeMCC: "404", TraceID: "trace-1", DeviceID: "dev-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("resolveVoWiFiCountryProxy(404)=%+v, want nil direct", got)
	}
}

func TestResolveVoWiFiCountryProxyCardOverride(t *testing.T) {
	openDeviceTestDB(t)
	loadDeviceCountryTableFixture(t)
	now := time.Now()
	if err := db.UpsertUpstreamProxy(db.UpstreamProxy{ID: "proxy-uk-1", Addr: "127.0.0.1:1081", Enabled: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertUpstreamProxy(db.UpstreamProxy{ID: "proxy-uk-2", Addr: "127.0.0.1:1082", Enabled: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertUpstreamProxyCountryRule(db.UpstreamProxyCountryRule{CountryCode: "US", UpstreamProxyID: "proxy-uk-1", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	iccid := "8944101111111111111"
	if err := db.UpsertCardPolicy(db.CardPolicy{ICCID: iccid, VowifiUpstreamProxyID: "proxy-uk-2", Source: "user"}); err != nil {
		t.Fatal(err)
	}
	got, err := resolveVoWiFiCountryProxy(voWiFiProxyResolveRequest{
		HomeMCC: "310", TraceID: "trace-1", DeviceID: "dev-1", ICCID: iccid,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != "proxy-uk-2" {
		t.Fatalf("card override=%+v, want proxy-uk-2", got)
	}
}

func TestResolveVoWiFiCountryProxyCardDirect(t *testing.T) {
	openDeviceTestDB(t)
	loadDeviceCountryTableFixture(t)
	now := time.Now()
	if err := db.UpsertUpstreamProxy(db.UpstreamProxy{ID: "proxy-us", Addr: "127.0.0.1:1080", Enabled: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertUpstreamProxyCountryRule(db.UpstreamProxyCountryRule{CountryCode: "US", UpstreamProxyID: "proxy-us", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	iccid := "8944102222222222222"
	if err := db.UpsertCardPolicy(db.CardPolicy{ICCID: iccid, VowifiUpstreamProxyID: db.VoWiFiUpstreamProxyDirect, Source: "user"}); err != nil {
		t.Fatal(err)
	}
	got, err := resolveVoWiFiCountryProxy(voWiFiProxyResolveRequest{
		HomeMCC: "310", TraceID: "trace-1", DeviceID: "dev-1", ICCID: iccid,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("card direct=%+v, want nil", got)
	}
}

func TestPickCountryPoolProxySkipsUDPUnhealthyWhenHealthyPeerExists(t *testing.T) {
	proxies := []db.UpstreamProxy{
		{ID: "gb-bad", Addr: "sichuan.example:2260", Enabled: true},
		{ID: "gb-ok", Addr: "127.0.0.1:7890", Enabled: true},
	}
	probe := func(_ context.Context, proxy db.UpstreamProxy) (upstreamproxy.ProbeResult, error) {
		if proxy.ID == "gb-ok" {
			return upstreamproxy.ProbeResult{
				Reachable: true, HandshakeOK: true, UDPAssociateOK: true, UDPRelayOK: true,
			}, nil
		}
		return upstreamproxy.ProbeResult{
			Reachable: true, HandshakeOK: true, UDPAssociateOK: true, UDPRelayOK: false,
		}, errors.New("udp relay failed")
	}
	for i := 0; i < 20; i++ {
		got := pickCountryPoolProxyWith(context.Background(), proxies, probe)
		if got == nil || got.ID != "gb-ok" {
			t.Fatalf("pick=%+v, want gb-ok when a UDP-healthy peer exists", got)
		}
	}
}

func TestPickCountryPoolProxyRandomizesAmongUDPHealthyPeers(t *testing.T) {
	proxies := []db.UpstreamProxy{
		{ID: "gb-a", Addr: "127.0.0.1:1081", Enabled: true},
		{ID: "gb-b", Addr: "127.0.0.1:1082", Enabled: true},
	}
	probe := func(_ context.Context, proxy db.UpstreamProxy) (upstreamproxy.ProbeResult, error) {
		return upstreamproxy.ProbeResult{
			Reachable: true, HandshakeOK: true, UDPAssociateOK: true, UDPRelayOK: true,
		}, nil
	}
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		got := pickCountryPoolProxyWith(context.Background(), proxies, probe)
		if got == nil {
			t.Fatal("pick returned nil")
		}
		seen[got.ID] = true
	}
	if !seen["gb-a"] || !seen["gb-b"] {
		t.Fatalf("healthy pool should still randomize, got %v", seen)
	}
}

func TestPickCountryPoolProxyFallsBackWhenUDPFailsOnEveryNode(t *testing.T) {
	proxies := []db.UpstreamProxy{
		{ID: "gb-a", Addr: "127.0.0.1:1081", Enabled: true},
		{ID: "gb-b", Addr: "127.0.0.1:1082", Enabled: true},
	}
	probe := func(_ context.Context, proxy db.UpstreamProxy) (upstreamproxy.ProbeResult, error) {
		return upstreamproxy.ProbeResult{
			Reachable: true, HandshakeOK: true, UDPAssociateOK: true, UDPRelayOK: false,
		}, errors.New("udp relay failed")
	}
	got := pickCountryPoolProxyWith(context.Background(), proxies, probe)
	if got == nil || (got.ID != "gb-a" && got.ID != "gb-b") {
		t.Fatalf("all-UDP-fail pool should still pick a node, got %+v", got)
	}
}

func TestPickCountryPoolProxySingleNodeKeepsUDPUnhealthy(t *testing.T) {
	proxies := []db.UpstreamProxy{
		{ID: "gb-only", Addr: "127.0.0.1:1081", Enabled: true},
	}
	probe := func(_ context.Context, proxy db.UpstreamProxy) (upstreamproxy.ProbeResult, error) {
		t.Fatal("single-node country pick must not probe")
		return upstreamproxy.ProbeResult{}, errors.New("unused")
	}
	got := pickCountryPoolProxyWith(context.Background(), proxies, probe)
	if got == nil || got.ID != "gb-only" {
		t.Fatalf("single node=%+v, want gb-only", got)
	}
}

func TestResolveVoWiFiCountryProxyDoesNotFailOpenPinnedRoute(t *testing.T) {
	openDeviceTestDB(t)
	loadDeviceCountryTableFixture(t)
	now := time.Now()
	if err := db.UpsertUpstreamProxy(db.UpstreamProxy{
		ID: "country-node", Addr: "127.0.0.1:1081", Enabled: true, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertUpstreamProxyCountryRule(db.UpstreamProxyCountryRule{
		CountryCode: "US", UpstreamProxyID: "country-node", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	iccid := "8944103333333333333"
	if err := db.UpsertCardPolicy(db.CardPolicy{
		ICCID: iccid, VowifiUpstreamProxyID: "missing-node", Source: "user",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := resolveVoWiFiCountryProxy(voWiFiProxyResolveRequest{
		HomeMCC: "310", TraceID: "trace-1", DeviceID: "dev-1", ICCID: iccid,
	})
	if err == nil || got != nil {
		t.Fatalf("pinned missing route resolved to %+v with err=%v", got, err)
	}
}
