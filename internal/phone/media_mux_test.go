package phone

import (
	"net"
	"testing"
)

type fakeUDPMux struct {
	addrs []net.Addr
}

func (f *fakeUDPMux) Close() error                                    { return nil }
func (f *fakeUDPMux) GetConn(string, net.Addr) (net.PacketConn, error) { return nil, nil }
func (f *fakeUDPMux) RemoveConnByUfrag(string)                        {}
func (f *fakeUDPMux) GetListenAddresses() []net.Addr                  { return f.addrs }

func mustAddr(t *testing.T, value string) net.Addr {
	t.Helper()
	addr, err := net.ResolveUDPAddr("udp", value)
	if err != nil {
		t.Fatalf("resolve %s: %v", value, err)
	}
	return addr
}

// TestPhysicalMuxKeepsOnlyPhysicalInterfaceAddresses 钉住候选收敛：
// pion 在未指定地址上会枚举全部网卡，docker 网桥（172.x）和模组 wwan
// （10.126.x）都会变成 host 候选，ICE 选中它们就会把媒体带偏。
func TestPhysicalMuxKeepsOnlyPhysicalInterfaceAddresses(t *testing.T) {
	base := &fakeUDPMux{addrs: []net.Addr{
		mustAddr(t, "192.168.3.30:7580"),
		mustAddr(t, "172.17.0.1:7580"),
		mustAddr(t, "10.126.126.2:7580"),
		mustAddr(t, "[240e:399:ea2:a970::1]:7580"),
	}}
	mux := &physicalMux{UDPMux: base, allowed: map[string]struct{}{
		"192.168.3.30":               {},
		"240e:399:ea2:a970::1":       {},
	}}

	got := mux.GetListenAddresses()
	if len(got) != 2 {
		t.Fatalf("kept %d candidates, want 2: %v", len(got), got)
	}
	for _, addr := range got {
		host, _, err := net.SplitHostPort(addr.String())
		if err != nil {
			t.Fatal(err)
		}
		switch host {
		case "192.168.3.30", "240e:399:ea2:a970::1":
		default:
			t.Fatalf("unexpected candidate kept: %s", addr)
		}
	}
}

// TestPhysicalMuxFallsBackWhenNothingSurvives 过滤后一个不剩时必须退回原始
// 列表，否则 ICE 会没有任何候选可用。
func TestPhysicalMuxFallsBackWhenNothingSurvives(t *testing.T) {
	base := &fakeUDPMux{addrs: []net.Addr{
		mustAddr(t, "172.17.0.1:7580"),
		mustAddr(t, "10.126.126.2:7580"),
	}}
	mux := &physicalMux{UDPMux: base, allowed: map[string]struct{}{"192.168.3.30": {}}}

	if got := mux.GetListenAddresses(); len(got) != 2 {
		t.Fatalf("fallback should return all candidates, got %v", got)
	}
}

// TestPhysicalMuxWithoutAllowListPassesThrough 收集不到物理网卡地址时不做
// 过滤，保持原行为。
func TestPhysicalMuxWithoutAllowListPassesThrough(t *testing.T) {
	base := &fakeUDPMux{addrs: []net.Addr{mustAddr(t, "172.17.0.1:7580")}}
	mux := &physicalMux{UDPMux: base}

	if got := mux.GetListenAddresses(); len(got) != 1 {
		t.Fatalf("expected pass-through, got %v", got)
	}
}
