package volte

import (
	"context"
	"errors"
	"testing"

	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
)

const qpcmvCommand = "AT+QPCMV=1,2"

// countFakeCommands 统计某个 AT 命令被下发的次数。
func countFakeCommands(host *FakeModem, want string) int {
	host.mu.Lock()
	defer host.mu.Unlock()
	count := 0
	for _, cmd := range host.Commands {
		if cmd == want {
			count++
		}
	}
	return count
}

// TestVoicePCMEnableIsOneShotArmIsPerCall 钉住 QPCMV 的两档语义：
// enable 阶段只发一次（不重复打扰模组），而每次拨号前必须重新下发。
//
// 背景：模组重启（USB 重新枚举）会丢掉 USB 语音路由，而进程里的 qpcmvTried
// 仍是 true，于是再也不补发——声卡能打开、下行正常，上行每帧写都 EPIPE，
// 对方完全听不到。切换一次 phone_mode 能恢复，正是因为它重建了会话。
func TestVoicePCMEnableIsOneShotArmIsPerCall(t *testing.T) {
	host := newFakeModem()
	host.Audio = "hw:3,0"
	ctl := NewControllerWithBackup(host, t.TempDir())
	if err := ctl.Enable(context.Background(), "wwan1"); err != nil {
		t.Fatal(err)
	}
	if got := countFakeCommands(host, qpcmvCommand); got != 1 {
		t.Fatalf("enable 阶段应下发一次 QPCMV，实际 %d", got)
	}

	// enable 之后再走一次 ensure（callPCM 会调）不应重复下发。
	ctl.ensureVoicePCM("wwan1")
	if got := countFakeCommands(host, qpcmvCommand); got != 1 {
		t.Fatalf("enable 后重复 ensureVoicePCM 不应再下发，实际 %d", got)
	}

	// 每次拨号前都要补发，不受一次性闸门限制。
	for i := 1; i <= 2; i++ {
		ctl.armVoicePCM("wwan1")
		if got := countFakeCommands(host, qpcmvCommand); got != 1+i {
			t.Fatalf("第 %d 次拨号前应补发 QPCMV，实际总计 %d", i, got)
		}
	}
}

// TestBeginCallRearmsVoicePCM 确认补发挂在拨号路径上：每通外呼都会多发一次。
func TestBeginCallRearmsVoicePCM(t *testing.T) {
	host := newFakeModem()
	host.Audio = "hw:3,0"
	ctl := NewControllerWithBackup(host, t.TempDir())
	if err := ctl.Enable(context.Background(), "wwan1"); err != nil {
		t.Fatal(err)
	}
	before := countFakeCommands(host, qpcmvCommand)

	if _, err := ctl.BeginCall(context.Background(), voicehost.BeginCallRequest{
		DeviceID: "wwan1", Callee: "10010",
	}); err != nil {
		t.Fatal(err)
	}
	if got := countFakeCommands(host, qpcmvCommand); got != before+1 {
		t.Fatalf("拨号前应补发一次 QPCMV：before=%d after=%d", before, got)
	}
}

// TestArmVoicePCMSkipsWithoutModemSoundCard 没有模组声卡时不应盲发 AT，
// 否则每次拨号都会多一条注定失败的指令。
func TestArmVoicePCMSkipsWithoutModemSoundCard(t *testing.T) {
	host := newFakeModem()
	host.Audio = ""
	ctl := NewControllerWithBackup(host, t.TempDir())
	if err := ctl.Enable(context.Background(), "wwan1"); err != nil {
		t.Fatal(err)
	}
	before := countFakeCommands(host, qpcmvCommand)

	ctl.armVoicePCM("wwan1")
	if got := countFakeCommands(host, qpcmvCommand); got != before {
		t.Fatalf("无声卡时不应下发 QPCMV：before=%d after=%d", before, got)
	}
}

// TestVoicePCMFailureKeepsAudioAvailable 钉住"保险失败不能毁掉通话"：
// QPCMV 下发失败（AT 通道忙、超时）只保留状态位与告警，绝不能置
// alsaUnavailable —— 那会让 callPCM 直接返回 nullPCM，整通电话彻底无音频。
func TestVoicePCMFailureKeepsAudioAvailable(t *testing.T) {
	host := newFakeModem()
	host.Audio = "hw:3,0"
	host.Fail = map[string]error{qpcmvCommand: errors.New("at timeout")}
	ctl := NewControllerWithBackup(host, t.TempDir())
	_ = ctl.Enable(context.Background(), "wwan1")

	if ctl.alsaUnavailable("wwan1") {
		t.Fatal("QPCMV 失败不应禁用声卡音频")
	}
	if !ctl.Status("wwan1").QPCMVFailed {
		t.Fatal("QPCMV 失败仍应体现在状态里")
	}

	// 拨号路径上的补发同样不能因为失败而禁用音频。
	ctl.armVoicePCM("wwan1")
	if ctl.alsaUnavailable("wwan1") {
		t.Fatal("拨号前补发 QPCMV 失败不应禁用声卡音频")
	}
}
