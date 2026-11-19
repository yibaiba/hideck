package device

import (
	"context"
	"strings"
	"time"

	"github.com/yibaiba/hideck/internal/modem"
)

// probeIMEICachedFn 允许测试替换底层 IMEI 探测实现。
var probeIMEICachedFn = modem.ProbeIMEICached
var probeIMEICachedContextFn = modem.ProbeIMEICachedContext
var probeProfileIMEIFn = modem.ATProfile.ProbeIMEI

// orderedATPortCandidates 按“单设备定界”规则生成探测顺序。
// 只允许在当前设备自己的 ATPorts 集合内排序和前置候选口；
// 如果 candidate 不属于该设备 ATPorts，则直接忽略，避免跨设备误探测。
func orderedATPortCandidates(candidate string, atPorts []string) []string {
	ports := sortATPortCandidates(atPorts)
	candidate = strings.TrimSpace(candidate)

	if len(ports) == 0 {
		if candidate == "" {
			return nil
		}
		return []string{candidate}
	}

	if candidate == "" {
		return ports
	}

	out := make([]string, 0, len(ports))
	if containsPort(ports, candidate) {
		out = append(out, candidate)
	}
	for _, port := range ports {
		if port == candidate {
			continue
		}
		out = append(out, port)
	}
	return out
}

// ResolveATPortForDevice 只探测某一台设备自己的候选 AT 口。
// 返回第一个成功读到 IMEI 的端口；若全部失败，则返回空值。
func ResolveATPortForDevice(candidate string, atPorts []string, timeout time.Duration) (atPort, imei string) {
	for _, port := range orderedATPortCandidates(candidate, atPorts) {
		imei, err := probeIMEICachedFn(port, timeout)
		if err == nil && imei != "" {
			return port, imei
		}
	}
	return "", ""
}

// ResolveATPortForDeviceContext 限定在单台设备端口内探测，并响应 USB 重枚举取消。
func ResolveATPortForDeviceContext(
	ctx context.Context,
	candidate string,
	atPorts []string,
	timeout time.Duration,
) (atPort, imei string) {
	for _, port := range orderedATPortCandidates(candidate, atPorts) {
		imei, err := probeIMEICachedContextFn(ctx, port, timeout)
		if err == nil && imei != "" {
			return port, imei
		}
		if ctx.Err() != nil {
			return "", ""
		}
	}
	return "", ""
}

func containsPort(ports []string, target string) bool {
	for _, port := range ports {
		if port == target {
			return true
		}
	}
	return false
}

// ResolveQMIDeviceATPort 为 QMI 设备解析真实可用的主 AT 口。
// 解析范围严格限制在该设备的 ATPorts 内，并将成功探测到的 IMEI 回填到设备对象。
func ResolveQMIDeviceATPort(dev QMIDevice, timeout time.Duration) (QMIDevice, string) {
	atPort, imei := ResolveATPortForDevice(dev.ATPort, dev.ATPorts, timeout)
	dev.ATPort = atPort
	return dev, imei
}

// ResolveCompatibleModemATPort 为兼容发现结果解析真实可用的主 AT 口。
// 行为与 QMI 设备一致，同样只允许在该设备自己的 ATPorts 范围内探测。
func ResolveCompatibleModemATPort(dev CompatibleModem, timeout time.Duration) (CompatibleModem, string) {
	if atDeviceRulesFor(dev.ATProfile()).verifyLiveIMEI {
		return ResolveCompatibleModemATPortContext(context.Background(), dev, timeout)
	}
	atPort, imei := ResolveATPortForDevice(dev.ATPort, dev.ATPorts, timeout)
	dev.ATPort = atPort
	if imei != "" {
		dev.IMEI = imei
	}
	return dev, imei
}

// ResolveCompatibleModemATPortContext 是后端切换发现器使用的可取消版本。
func ResolveCompatibleModemATPortContext(
	ctx context.Context,
	dev CompatibleModem,
	timeout time.Duration,
) (CompatibleModem, string) {
	profile := dev.ATProfile()
	if atDeviceRulesFor(profile).verifyLiveIMEI {
		preferred := dev.ATPort
		dev.ATPort, dev.IMEI = "", ""
		for _, port := range orderedATPortCandidates(preferred, dev.ATPorts) {
			imei, err := probeProfileIMEIFn(profile, ctx, port, timeout)
			if err == nil && imei != "" {
				dev.ATPort, dev.IMEI = port, imei
				return dev, imei
			}
			if ctx.Err() != nil {
				break
			}
		}
		return dev, ""
	}
	atPort, imei := ResolveATPortForDeviceContext(ctx, dev.ATPort, dev.ATPorts, timeout)
	dev.ATPort = atPort
	if imei != "" {
		dev.IMEI = imei
	}
	return dev, imei
}
