package device

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/iniwex5/vowifi-go/runtimehost"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/vowifihost"
)

// Only a running runtime that holds the card is restarted or woken; a device
// without a running runtime is not started, and other cards are left alone
// (hideck#102).
func TestRedetectMWIForICCIDTouchesOnlyRunningRuntimesOfTheCard(t *testing.T) {
	for _, reconnect := range []bool{false, true} {
		pool := NewPool(&config.Config{})
		host := pool.voWiFiHost()
		starts := 0
		host.SetRuntimeStartForTest(func(context.Context, runtimehost.StartRequest) (*runtimehost.Instance, error) {
			starts++
			return &runtimehost.Instance{}, nil
		})
		var mu sync.Mutex
		var restarted []string
		host.SetLifecycleRunForTest(func(_ context.Context, cmd vowifihost.LifecycleCommand) error {
			mu.Lock()
			defer mu.Unlock()
			restarted = append(restarted, cmd.DeviceID)
			return nil
		})
		for _, dev := range []struct {
			id, iccid string
			running   bool
		}{
			{"dev1", "89490200000000000019", true},
			{"dev2", "89490200000000000019", false},
			{"dev3", "89490200000000000027", true},
		} {
			worker := &Worker{ID: dev.id, Config: config.DeviceConfig{ID: dev.id, VoWiFiEnabled: true}}
			worker.state.Identity.ICCID = dev.iccid
			pool.workers[dev.id] = worker
			if dev.running {
				host.RuntimeStore().SetInstance(dev.id, &runtimehost.Instance{})
			}
		}
		if err := pool.RedetectMWIForICCID("89490200000000000019F", reconnect); err != nil {
			t.Fatal(err)
		}
		if err := pool.RedetectMWIForICCID("", reconnect); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		sort.Strings(restarted)
		got := strings.Join(restarted, ",")
		mu.Unlock()
		want := ""
		if reconnect {
			want = "dev1"
		}
		if got != want || starts != 0 {
			t.Fatalf("reconnect=%t: restarted=%q starts=%d", reconnect, got, starts)
		}
		pool.cancel()
	}
}
