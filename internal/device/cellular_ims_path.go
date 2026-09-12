package device

import (
	"fmt"
	"strings"

	"github.com/iniwex5/vowifi-go/runtimehost"
)

type cellularIMSTransport struct {
	BindInterface string
	Proxy         *runtimehost.ProxyConfig
	ViaProxy      bool
}

func classifyCellularIMSError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "无法访问外网") {
		return err
	}
	if strings.Contains(msg, "ePDG") || strings.Contains(msg, "SWu tunnel") {
		return fmt.Errorf("蜂窝数据无法到达运营商 ePDG: %w", err)
	}
	return err
}

func cellularIMSUsesBoundInterface(hasInternet bool, iface string) bool {
	return hasInternet && strings.TrimSpace(iface) != ""
}

func selectCellularIMSTransport(hasInternet bool, iface string, proxy *runtimehost.ProxyConfig) (cellularIMSTransport, error) {
	iface = strings.TrimSpace(iface)
	proxyReady := proxy != nil && proxy.Enabled && strings.TrimSpace(proxy.Addr) != ""
	if cellularIMSUsesBoundInterface(hasInternet, iface) {
		return cellularIMSTransport{BindInterface: iface}, nil
	}
	if proxyReady {
		return cellularIMSTransport{Proxy: proxy, ViaProxy: true}, nil
	}
	if !hasInternet {
		return cellularIMSTransport{}, fmt.Errorf("蜂窝数据已连接但无法访问外网，无法到达 ePDG")
	}
	return cellularIMSTransport{}, nil
}
