package modem

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"
)

// imeiCacheItem 存储 IMEI 缓存条目及对应的获取时间戳
type imeiCacheItem struct {
	IMEI string
	TS   time.Time
	Node os.FileInfo
}

// imeiCache 提供线程安全的内存 IMEI 映射缓存，避免频繁通过串口发起硬件查询
var imeiCache struct {
	mu sync.RWMutex
	m  map[string]imeiCacheItem
}

// ProbeIMEICached 在 10 分钟缓存有效期内优先从内存缓存中获取指定 AT 串口的 IMEI；若未命中或过期，则调用底层串口方法探测
func ProbeIMEICached(atPort string, timeout time.Duration) (string, error) {
	return ProbeIMEICachedContext(context.Background(), atPort, timeout)
}

// ProbeIMEICachedContext 提供可取消的缓存 IMEI 探测。
func ProbeIMEICachedContext(ctx context.Context, atPort string, timeout time.Duration) (string, error) {
	atPort = strings.TrimSpace(atPort)
	if atPort == "" {
		return "", errors.New("empty at port")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	if imei := loadCachedIMEI(atPort); imei != "" {
		return imei, nil
	}

	node, _ := os.Stat(atPort)
	imei, err := ProbeIMEIContext(ctx, atPort, timeout)
	if err == nil && imei != "" {
		storeCachedIMEI(atPort, imei, node)
	}
	return imei, err
}

func loadCachedIMEI(atPort string) string {
	imeiCache.mu.RLock()
	defer imeiCache.mu.RUnlock()
	item, ok := imeiCache.m[atPort]
	node, err := os.Stat(atPort)
	if !ok || item.IMEI == "" || time.Since(item.TS) >= 10*time.Minute || err != nil || item.Node == nil || !os.SameFile(item.Node, node) || !item.Node.ModTime().Equal(node.ModTime()) {
		return ""
	}
	return item.IMEI
}

func storeCachedIMEI(atPort, imei string, node os.FileInfo) {
	current, err := os.Stat(atPort)
	if err != nil || node == nil || !os.SameFile(node, current) || !node.ModTime().Equal(current.ModTime()) {
		return
	}
	imeiCache.mu.Lock()
	defer imeiCache.mu.Unlock()
	if imeiCache.m == nil {
		imeiCache.m = make(map[string]imeiCacheItem)
	}
	imeiCache.m[atPort] = imeiCacheItem{IMEI: imei, TS: time.Now(), Node: node}
}

// ProbeIMEI 通过打开底层 TTY 串口设备并执行 `AT+CGSN` 指令来实时探测模组的 IMEI 串号
func ProbeIMEI(atPort string, timeout time.Duration) (string, error) {
	return ProbeIMEIContext(context.Background(), atPort, timeout)
}

// ProbeIMEIContext 在取消时主动关闭串口，避免 USB 重枚举期间阻塞设备恢复。
func ProbeIMEIContext(ctx context.Context, atPort string, timeout time.Duration) (string, error) {
	atPort = strings.TrimSpace(atPort)
	if atPort == "" {
		return "", errors.New("empty at port")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = 1500 * time.Millisecond
	}

	// 配置标准的 3 线异步串口波特率与帧校验格式
	mode := &serial.Mode{
		BaudRate: 115200,
		DataBits: 8,
		StopBits: serial.OneStopBit,
		Parity:   serial.NoParity,
	}

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	p, err := openIMEISerialPort(probeCtx, atPort, mode)
	if err != nil {
		return "", err
	}
	defer p.Close()
	stopClose := make(chan struct{})
	go closeIMEISerialPortOnCancel(probeCtx, p, stopClose)
	defer close(stopClose)

	_ = p.SetReadTimeout(80 * time.Millisecond)

	buf := make([]byte, 1024)
	var acc strings.Builder

	write := func(s string) {
		_, _ = p.Write([]byte(s))
	}

	// 写入 AT 测试命令与查询 IMEI 的 AT+CGSN 命令
	write("AT\r\n")
	time.Sleep(40 * time.Millisecond)
	write("AT+CGSN\r\n")

	// 在指定的截止时间内轮询并解析串口输出内容
	for probeCtx.Err() == nil {
		n, rerr := p.Read(buf)
		if n > 0 {
			acc.Write(buf[:n])
			if imei := parseIMEI(acc.String()); imei != "" {
				return imei, nil
			}
		}
		if rerr != nil {
			if probeCtx.Err() != nil {
				return "", probeCtx.Err()
			}
			if strings.Contains(strings.ToLower(rerr.Error()), "timeout") {
				continue
			}
		}
	}

	// 最终尝试解析一次累积的串口缓冲区
	if imei := parseIMEI(acc.String()); imei != "" {
		return imei, nil
	}
	if err := probeCtx.Err(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return "", err
	}
	return "", errors.New("imei probe timeout")
}

type imeiSerialOpenResult struct {
	port serial.Port
	err  error
}

func openIMEISerialPort(ctx context.Context, atPort string, mode *serial.Mode) (serial.Port, error) {
	result := make(chan imeiSerialOpenResult, 1)
	go func() {
		port, err := serial.Open(atPort, mode)
		result <- imeiSerialOpenResult{port: port, err: err}
	}()
	select {
	case opened := <-result:
		return opened.port, opened.err
	case <-ctx.Done():
		go closeLateIMEISerialPort(result)
		return nil, ctx.Err()
	}
}

func closeLateIMEISerialPort(result <-chan imeiSerialOpenResult) {
	opened := <-result
	if opened.port != nil {
		_ = opened.port.Close()
	}
}

func closeIMEISerialPortOnCancel(ctx context.Context, port serial.Port, stop <-chan struct{}) {
	select {
	case <-ctx.Done():
		_ = port.Close()
	case <-stop:
	}
}
