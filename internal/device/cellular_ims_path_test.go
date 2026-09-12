package device

import (
	"fmt"
	"strings"
	"testing"

	"github.com/iniwex5/vowifi-go/runtimehost"
)

func TestCellularIMSUsesBoundInterface(t *testing.T) {
	if !cellularIMSUsesBoundInterface(true, "wwan0") {
		t.Fatal("online iface should bind")
	}
	if cellularIMSUsesBoundInterface(true, "  ") {
		t.Fatal("blank iface should not bind")
	}
	if cellularIMSUsesBoundInterface(false, "wwan0") {
		t.Fatal("offline cellular should not bind")
	}
}

func TestSelectCellularIMSTransportPrefersInterfaceWhenOnline(t *testing.T) {
	got, err := selectCellularIMSTransport(true, "wwan0", &runtimehost.ProxyConfig{
		Enabled: true, Addr: "127.0.0.1:1080",
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got.BindInterface != "wwan0" || got.ViaProxy || got.Proxy != nil {
		t.Fatalf("got %+v, want bind wwan0 without proxy", got)
	}
}

func TestSelectCellularIMSTransportFallsBackToCountryProxy(t *testing.T) {
	proxy := &runtimehost.ProxyConfig{Enabled: true, Addr: "10.0.0.2:1080"}
	got, err := selectCellularIMSTransport(false, "wwan0", proxy)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !got.ViaProxy || got.Proxy != proxy || got.BindInterface != "" {
		t.Fatalf("got %+v, want country proxy fallback", got)
	}
}

func TestClassifyCellularIMSErrorKeepsOuterNetFailure(t *testing.T) {
	err := fmt.Errorf("蜂窝数据已连接但无法访问外网，无法到达 ePDG")
	if got := classifyCellularIMSError(err); got != err {
		t.Fatalf("got %v", got)
	}
}

func TestClassifyCellularIMSErrorWrapsEPDGTimeout(t *testing.T) {
	err := classifyCellularIMSError(fmt.Errorf("runtimehost: SWu tunnel establishment failed: 等待 ePDG 隧道建立超时"))
	if err == nil || !strings.Contains(err.Error(), "无法到达运营商 ePDG") {
		t.Fatalf("err=%v", err)
	}
}

func TestSelectCellularIMSTransportErrorsWithoutInternetOrProxy(t *testing.T) {
	_, err := selectCellularIMSTransport(false, "wwan0", nil)
	if err == nil || !strings.Contains(err.Error(), "无法访问外网") {
		t.Fatalf("err=%v, want 无法访问外网", err)
	}
}
