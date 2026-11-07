package device

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yibaiba/hideck/internal/backend"
	"github.com/yibaiba/hideck/internal/cardpolicy"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/vowifihost"
	"github.com/yibaiba/hideck/pkg/logger"
)

func TestApplyPolicyCellularAirplaneKeepsSoftwarePhone(t *testing.T) {
	w := &Worker{ID: "wwan0"}
	applyPolicyToWorker(w, cardpolicy.Policy{
		ICCID: "x", VoWiFiEnabled: true, PhoneMode: "cellular",
		AirplaneEnabled: true, NetworkEnabled: true, DataStrategy: "on_demand",
	})
	if !w.Config.AirplaneEnabled || !w.Config.VoWiFiEnabled || w.Config.NetworkEnabled {
		t.Fatalf("蜂窝飞行应保留软件电话并关流量: %+v", w.Config)
	}
	if !w.cellularRadioIsSuppressed() {
		t.Fatal("蜂窝飞行应抑制射频")
	}
}

func TestApplyPolicyChinaUnicomForcesNativeVoLTE(t *testing.T) {
	w := &Worker{ID: "wwan1"}
	w.state.Identity.NativeMCC = "460"
	w.state.Identity.IMSI = "460011234567890"
	applyPolicyToWorker(w, cardpolicy.Policy{
		ICCID: "x", VoWiFiEnabled: true, PhoneMode: "cellular", DataStrategy: "on_demand",
	})
	if w.Config.PhoneMode != PhoneModeVoLTE {
		t.Fatalf("PhoneMode=%q want volte", w.Config.PhoneMode)
	}
	if w.Config.AirplaneEnabled || w.cellularRadioIsSuppressed() {
		t.Fatalf("中国卡软件 IMS 不可用时应驻网走 VoLTE: %+v suppressed=%v", w.Config, w.cellularRadioIsSuppressed())
	}
}

func TestApplyPolicyVoLTECampsWithoutForcingData(t *testing.T) {
	w := &Worker{ID: "wwan1"}
	applyPolicyToWorker(w, cardpolicy.Policy{
		ICCID: "x", VoWiFiEnabled: true, PhoneMode: "volte",
	})
	if w.Config.AirplaneEnabled || w.Config.NetworkEnabled || w.cellularRadioIsSuppressed() {
		t.Fatalf("VoLTE 应驻网且不强制开流量: %+v suppressed=%v", w.Config, w.cellularRadioIsSuppressed())
	}
	if w.Config.PhoneMode != "volte" {
		t.Fatalf("PhoneMode=%q", w.Config.PhoneMode)
	}
}

func TestApplyPolicyCellularIdleKeepsCamped(t *testing.T) {
	w := &Worker{ID: "wwan0"}
	applyPolicyToWorker(w, cardpolicy.Policy{
		ICCID: "x", VoWiFiEnabled: true, PhoneMode: "cellular", DataStrategy: "on_demand",
	})
	if w.Config.AirplaneEnabled || w.Config.NetworkEnabled || w.cellularRadioIsSuppressed() {
		t.Fatalf("蜂窝未开流量应保持驻网: %+v suppressed=%v", w.Config, w.cellularRadioIsSuppressed())
	}

	applyPolicyToWorker(w, cardpolicy.Policy{
		ICCID: "x", VoWiFiEnabled: true, PhoneMode: "cellular", DataStrategy: "on_demand", NetworkEnabled: true,
	})
	if w.Config.AirplaneEnabled || !w.Config.NetworkEnabled || w.cellularRadioIsSuppressed() {
		t.Fatalf("打开网络后应保持在线并允许数据: %+v suppressed=%v", w.Config, w.cellularRadioIsSuppressed())
	}
}

func TestApplyPolicyLocksLebaraUKRadioWithoutRewritingIntent(t *testing.T) {
	w := &Worker{ID: "wwan-lebara"}
	w.state.Identity.IMSI = "234870000000001"
	w.state.Identity.ICCID = "8944000000000000087"
	pol := cardpolicy.Policy{
		ICCID: "8944000000000000087", AirplaneEnabled: false, NetworkEnabled: true,
		PhoneMode: "cellular", VoWiFiEnabled: true, DataStrategy: "always",
	}
	if err := applyPolicyToWorker(w, pol); err != nil {
		t.Fatal(err)
	}
	if !w.Config.AirplaneEnabled || w.Config.NetworkEnabled || w.Config.PhoneMode != "wifi" {
		t.Fatalf("Lebara 运行时应锁射频: %+v", w.Config)
	}
	if !w.cellularRadioIsSuppressed() {
		t.Fatal("Lebara 运行时应抑制射频")
	}
	if !pol.NetworkEnabled || pol.AirplaneEnabled || pol.PhoneMode != "cellular" {
		t.Fatalf("不得改写传入策略: %+v", pol)
	}
}

func TestResolveAndApplyPolicyKeepsLebaraUKRadioOff(t *testing.T) {
	p := &Pool{ctx: context.Background()}
	p.SetPolicyResolver(&stubPolicyResolver{pol: cardpolicy.Policy{
		ICCID: "8944000000000000087", AirplaneEnabled: false,
		NetworkEnabled: true, PhoneMode: "cellular",
	}})
	stub := &workerStatusBackendStub{opMode: backend.ModeRFOff}
	w := &Worker{ID: "wwan-lebara", Backend: stub}
	w.state.Identity.ICCID = "8944000000000000087"
	w.state.Identity.IMSI = "234870000000001"

	p.workers = map[string]*Worker{w.ID: w}
	result := p.resolveAndApplyPolicy(w, "test")
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	for _, mode := range stub.setOpModeCalls {
		if mode == backend.ModeOnline {
			t.Fatalf("Lebara policy projection restored Online: %v", stub.setOpModeCalls)
		}
	}
	if !w.Config.AirplaneEnabled || w.Config.PhoneMode != "wifi" || w.Config.NetworkEnabled {
		t.Fatalf("effective Lebara policy was not retained: %+v", w.Config)
	}
}

func TestApplyPolicyDoesNotLockBareVodafoneNL(t *testing.T) {
	w := &Worker{ID: "wwan-nl"}
	w.state.Identity.IMSI = "204040000000001"
	applyPolicyToWorker(w, cardpolicy.Policy{
		ICCID: "x", AirplaneEnabled: false, NetworkEnabled: true, PhoneMode: "cellular",
	})
	if w.Config.AirplaneEnabled || !w.Config.NetworkEnabled || w.Config.PhoneMode != "cellular" {
		t.Fatalf("光秃 20404 不应当 Lebara 锁: %+v", w.Config)
	}
}

func TestApplyPolicyProjectsFields(t *testing.T) {
	w := &Worker{ID: "wwan0"}
	applyPolicyToWorker(w, cardpolicy.Policy{
		ICCID: "x", NetworkEnabled: true, VoWiFiEnabled: true,
		AirplaneEnabled: true, IPVersion: "v4v6", APN: "ims",
	})
	if w.Config.NetworkEnabled || !w.Config.VoWiFiEnabled || !w.Config.AirplaneEnabled {
		t.Fatalf("WiFi calling 投影应关流量并保持飞行: %+v", w.Config)
	}
	if w.Config.IPVersion != "v4v6" || w.Config.APN != "ims" {
		t.Fatalf("ip/apn 未投影: %+v", w.Config)
	}
	if !w.Config.SMSEnabled {
		t.Fatal("SMS 应恒为 true")
	}
	if !w.cellularRadioIsSuppressed() {
		t.Fatal("VoWiFi/飞行策略必须抑制蜂窝射频协调")
	}
	if w.restoreNetworkAfterVoWiFi {
		t.Fatal("WiFi calling 投影后不得把启动失败恢复射频留成 true")
	}
}

func TestShouldEnterAirplaneOnDeviceBindForDefaultVoWiFi(t *testing.T) {
	if shouldEnterAirplaneOnDeviceBind(config.DeviceConfig{}) {
		t.Fatal("空静态配置不应在绑定时先飞")
	}
	if !shouldEnterAirplaneOnDeviceBind(config.DeviceConfig{AirplaneEnabled: true, VoWiFiEnabled: false, NetworkEnabled: false}) {
		t.Fatal("第一次添加设备默认飞行时绑定时必须关射频")
	}
	if !shouldEnterAirplaneOnDeviceBind(config.DeviceConfig{VoWiFiEnabled: true, PhoneMode: PhoneModeWiFi}) {
		t.Fatal("默认 VoWiFi 开启时绑定时必须关射频")
	}
	if !shouldEnterAirplaneOnDeviceBind(config.DeviceConfig{AirplaneEnabled: true, VoWiFiEnabled: true, PhoneMode: PhoneModeWiFi}) {
		t.Fatal("VoWiFi 已开也不能跳过绑定时关射频")
	}
	if shouldEnterAirplaneOnDeviceBind(config.DeviceConfig{VoWiFiEnabled: true, PhoneMode: PhoneModeCellular}) {
		t.Fatal("蜂窝软件电话绑定时应允许驻网")
	}
}

func defaultNewCardPolicy(iccid string) cardpolicy.Policy {
	return cardpolicy.Policy{
		ICCID:           iccid,
		NetworkEnabled:  false,
		VoWiFiEnabled:   false,
		AirplaneEnabled: true,
		IPVersion:       "v4",
		PhoneMode:       PhoneModeWiFi,
		DataStrategy:    "on_demand",
	}
}

func assertNoRadioOnline(t *testing.T, stub *workerStatusBackendStub, label string) {
	t.Helper()
	for _, mode := range stub.setOpModeCalls {
		if mode == backend.ModeOnline {
			t.Fatalf("%s 不得开射频: %+v", label, stub.setOpModeCalls)
		}
	}
}

func TestDefaultAirplaneAndNonVoWiFiNeverOpenRadioOrIMS(t *testing.T) {
	cases := []struct {
		name string
		pol  cardpolicy.Policy
	}{
		{name: "first-card-default", pol: defaultNewCardPolicy("898600000000000001")},
		{name: "airplane-vowifi-off", pol: cardpolicy.Policy{
			ICCID: "898600000000000002", AirplaneEnabled: true, VoWiFiEnabled: false, PhoneMode: PhoneModeWiFi,
		}},
		{name: "airplane-cellular-phone-off", pol: cardpolicy.Policy{
			ICCID: "898600000000000003", AirplaneEnabled: true, VoWiFiEnabled: false, PhoneMode: PhoneModeCellular,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPool(nil)
			defer p.cancel()
			p.SetPolicyResolver(&stubPolicyResolver{pol: tc.pol})
			commands := make(chan vowifihost.LifecycleCommand, 1)
			p.voWiFiHost().LifecycleControllerForTest().TestRun = func(ctx context.Context, cmd vowifihost.LifecycleCommand) error {
				commands <- cmd
				return nil
			}
			stub := &workerStatusBackendStub{opMode: backend.ModeOnline}
			w := &Worker{ID: "wwan0", Backend: stub}
			w.state.Identity.ICCID = tc.pol.ICCID
			w.state.Identity.IMSI = "001010000000001"

			p.workers = map[string]*Worker{w.ID: w}
			res := p.resolveAndApplyPolicy(w, "startup_post_apply")
			if !res.Applied {
				t.Fatalf("应成功应用: %+v", res)
			}
			if !w.Config.AirplaneEnabled || w.Config.NetworkEnabled {
				t.Fatalf("默认飞行/VoWiFi 应关射频并关流量: %+v", w.Config)
			}
			if !w.cellularRadioIsSuppressed() {
				t.Fatal("应抑制蜂窝驻网")
			}
			assertNoRadioOnline(t, stub, tc.name)
			if len(stub.setOpModeCalls) != 1 || stub.setOpModeCalls[0] != backend.ModeRFOff {
				t.Fatalf("应从在线切到 RFOff: %+v", stub.setOpModeCalls)
			}

			select {
			case cmd := <-commands:
				t.Fatalf("默认飞行或未开 VoWiFi 不应启动 IMS/VoWiFi: %+v", cmd)
			case <-time.After(120 * time.Millisecond):
			}
		})
	}
}

func TestSwitchAirplaneToVoWiFiKeepsRadioOff(t *testing.T) {
	p := NewPool(nil)
	defer p.cancel()
	p.SetPolicyResolver(&stubPolicyResolver{pol: cardpolicy.Policy{
		ICCID: "898600000000000010", AirplaneEnabled: true, VoWiFiEnabled: true, PhoneMode: PhoneModeWiFi,
	}})
	commands := make(chan vowifihost.LifecycleCommand, 1)
	p.voWiFiHost().LifecycleControllerForTest().TestRun = func(ctx context.Context, cmd vowifihost.LifecycleCommand) error {
		commands <- cmd
		return nil
	}
	stub := &workerStatusBackendStub{opMode: backend.ModeRFOff}
	w := &Worker{
		ID:      "wwan0",
		Backend: stub,
		Config:  config.DeviceConfig{AirplaneEnabled: true, PhoneMode: PhoneModeWiFi},
	}
	w.state.Identity.ICCID = "898600000000000010"
	w.state.Identity.IMSI = "001010000000001"

	p.workers = map[string]*Worker{w.ID: w}
	res := p.resolveAndApplyPolicy(w, "api_enable_vowifi")
	if !res.Applied {
		t.Fatalf("应成功应用: %+v", res)
	}
	if !w.Config.AirplaneEnabled || w.Config.NetworkEnabled || !w.Config.VoWiFiEnabled {
		t.Fatalf("从飞行切到 VoWiFi 应继续关射频: %+v", w.Config)
	}
	assertNoRadioOnline(t, stub, "airplane-to-vowifi")
	if len(stub.setOpModeCalls) != 0 {
		t.Fatalf("已在飞行切到 VoWiFi 不应再动射频: %+v", stub.setOpModeCalls)
	}
}

func TestInitialCellularRadioSuppressionWaitsForLiveCardPolicy(t *testing.T) {
	p := NewPool(nil)
	defer p.cancel()
	p.SetPolicyResolver(&stubPolicyResolver{})

	if !p.initialCellularRadioSuppression(config.DeviceConfig{}) {
		t.Fatal("存在卡策略解析器时，真实 ICCID 策略投影前必须抑制蜂窝驻网")
	}

	w := &Worker{ID: "wwan0"}
	w.setCellularRadioSuppressed(p.initialCellularRadioSuppression(w.Config))
	applyPolicyToWorker(w, cardpolicy.Policy{ICCID: "123", NetworkEnabled: true})
	if w.cellularRadioIsSuppressed() {
		t.Fatal("在线卡策略投影后必须解除蜂窝驻网抑制")
	}
}

func TestInitialCellularRadioSuppressionWithoutResolverUsesDeviceConfig(t *testing.T) {
	p := NewPool(nil)
	defer p.cancel()

	if p.initialCellularRadioSuppression(config.DeviceConfig{}) {
		t.Fatal("无卡策略解析器且静态策略在线时不应抑制蜂窝驻网")
	}
	if !p.initialCellularRadioSuppression(config.DeviceConfig{VoWiFiEnabled: true}) {
		t.Fatal("静态 VoWiFi 策略必须抑制蜂窝驻网")
	}
}

func TestHoldRadioOffOnConnectDoesNotChangeStoredAirplane(t *testing.T) {
	p := &Pool{ctx: context.Background()}
	stub := &workerStatusBackendStub{opMode: backend.ModeOnline}
	w := &Worker{
		ID:      "wwan0",
		Backend: stub,
		Config:  config.DeviceConfig{AirplaneEnabled: false, ConnectHoldRF: true},
	}

	p.holdRadioOffOnConnect(w, "connect_hold_rf")

	if w.Config.AirplaneEnabled {
		t.Fatal("连接期先飞不得改写 AirplaneEnabled")
	}
	if !w.cellularRadioIsSuppressed() {
		t.Fatal("连接期先飞应暂扣驻网协调")
	}
	if len(stub.setOpModeCalls) != 1 || stub.setOpModeCalls[0] != backend.ModeRFOff {
		t.Fatalf("连接期第一条射频指令必须是 RFOff: %+v", stub.setOpModeCalls)
	}
	if w.Config.ConnectHoldRF {
		t.Fatal("成功 hold 后应清掉 ConnectHoldRF，避免 QMI 重试再飞")
	}
}

func TestHoldRadioOffOnConnectIdempotentWhenAlreadyFlight(t *testing.T) {
	p := &Pool{ctx: context.Background()}
	stub := &workerStatusBackendStub{opMode: backend.ModeRFOff}
	w := &Worker{ID: "wwan0", Backend: stub}

	w.Config.ConnectHoldRF = true
	p.holdRadioOffOnConnect(w, "connect_hold_rf")

	if len(stub.setOpModeCalls) != 0 {
		t.Fatalf("已在飞行不应重复切: %+v", stub.setOpModeCalls)
	}
	if w.Config.ConnectHoldRF {
		t.Fatal("已在飞行的 hold 也应清掉 ConnectHoldRF")
	}
}

func TestWithConnectHoldRFDoesNotPersistIntent(t *testing.T) {
	cfg := withConnectHoldRF(config.DeviceConfig{ID: "wwan0", AirplaneEnabled: false})
	if !cfg.ConnectHoldRF {
		t.Fatal("withConnectHoldRF 应打上运行时标记")
	}
	if cfg.AirplaneEnabled {
		t.Fatal("withConnectHoldRF 不得改飞行策略")
	}
}

func TestIdentityReadyAfterConnectHoldRestoresCamp(t *testing.T) {
	p := &Pool{ctx: context.Background()}
	p.SetPolicyResolver(&stubPolicyResolver{
		pol: cardpolicy.Policy{ICCID: "123", AirplaneEnabled: false, NetworkEnabled: false},
	})
	stub := &workerStatusBackendStub{opMode: backend.ModeOnline}
	w := &Worker{
		ID:      "wwan0",
		Backend: stub,
		Config:  config.DeviceConfig{AirplaneEnabled: false, ConnectHoldRF: true},
	}
	w.state.Identity.ICCID = "123"

	p.holdRadioOffOnConnect(w, "connect_hold_rf")
	p.workers = map[string]*Worker{w.ID: w}
	res := p.resolveAndApplyPolicy(w, "identity_ready")
	if !res.Applied {
		t.Fatalf("identity_ready 应应用策略: %+v", res)
	}
	if w.Config.AirplaneEnabled {
		t.Fatal("老卡驻网策略不得被写成飞行")
	}
	if len(stub.setOpModeCalls) < 2 || stub.setOpModeCalls[0] != backend.ModeRFOff || stub.setOpModeCalls[1] != backend.ModeOnline {
		t.Fatalf("先飞再按策略 Online: %+v", stub.setOpModeCalls)
	}
	if w.cellularRadioIsSuppressed() {
		t.Fatal("驻网策略投影后应解除射频抑制")
	}

	before := append([]backend.OperatingMode(nil), stub.setOpModeCalls...)
	w.Config.ConnectHoldRF = true
	p.applyAfterQMIControlReady(w, "qmi_core_recovered")
	if w.cellularRadioIsSuppressed() {
		t.Fatal("QMI 恢复时身份已在，不得再次抑制驻网")
	}
	if w.Config.AirplaneEnabled {
		t.Fatal("QMI 恢复不得把驻网卡写成飞行")
	}
	if w.Config.ConnectHoldRF {
		t.Fatal("身份已在时 QMI 恢复应清掉 ConnectHoldRF")
	}
	for i := len(before); i < len(stub.setOpModeCalls); i++ {
		if stub.setOpModeCalls[i] == backend.ModeRFOff {
			t.Fatalf("QMI 恢复不得再 RFOff: %+v", stub.setOpModeCalls)
		}
	}
}

func TestIdentityReadyAfterConnectHoldKeepsAirplaneOrVoWiFiRFOff(t *testing.T) {
	cases := []struct {
		name string
		pol  cardpolicy.Policy
	}{
		{name: "airplane", pol: cardpolicy.Policy{ICCID: "123", AirplaneEnabled: true}},
		{name: "vowifi-wifi", pol: cardpolicy.Policy{ICCID: "123", VoWiFiEnabled: true, PhoneMode: "wifi"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Pool{ctx: context.Background()}
			p.SetPolicyResolver(&stubPolicyResolver{pol: tc.pol})
			stub := &workerStatusBackendStub{opMode: backend.ModeOnline}
			w := &Worker{
				ID:      "wwan0",
				Backend: stub,
				Config:  config.DeviceConfig{AirplaneEnabled: false, ConnectHoldRF: true},
			}
			w.state.Identity.ICCID = "123"

			p.holdRadioOffOnConnect(w, "connect_hold_rf")
			p.workers = map[string]*Worker{w.ID: w}
			res := p.resolveAndApplyPolicy(w, "identity_ready")
			if !res.Applied {
				t.Fatalf("identity_ready 应应用策略: %+v", res)
			}
			for _, mode := range stub.setOpModeCalls {
				if mode == backend.ModeOnline {
					t.Fatalf("%s 策略投影后不应 Online: %+v", tc.name, stub.setOpModeCalls)
				}
			}
			if !w.cellularRadioIsSuppressed() {
				t.Fatalf("%s 应保持射频抑制", tc.name)
			}
		})
	}
}

// 投影时按策略真正进入飞行模式：当前在线 ⇒ 切 RFOff。
func TestProjectionEntersAirplaneMode(t *testing.T) {
	p := &Pool{ctx: context.Background()}
	stub := &workerStatusBackendStub{opMode: backend.ModeOnline}
	w := &Worker{ID: "wwan0", Backend: stub}

	p.enterAirplaneModeFromPolicy(w, "test")

	if len(stub.setOpModeCalls) != 1 || stub.setOpModeCalls[0] != backend.ModeRFOff {
		t.Fatalf("应切到 RFOff: %+v", stub.setOpModeCalls)
	}
}

func TestProjectionEnterAirplaneUpgradesLowPowerToPersistRFOff(t *testing.T) {
	p := &Pool{ctx: context.Background()}
	stub := &workerStatusBackendStub{opMode: backend.ModeLowPower}
	w := &Worker{ID: "wwan0", Backend: stub}

	p.enterAirplaneModeFromPolicy(w, "test")

	if len(stub.setOpModeCalls) != 1 || stub.setOpModeCalls[0] != backend.ModeRFOff {
		t.Fatalf("瞬时 LowPower 必须升级成持久 RFOff: %+v", stub.setOpModeCalls)
	}
}

func TestHoldRadioOffOnConnectUpgradesLowPowerToPersistRFOff(t *testing.T) {
	p := &Pool{ctx: context.Background()}
	stub := &workerStatusBackendStub{opMode: backend.ModeLowPower}
	w := &Worker{
		ID:      "wwan0",
		Backend: stub,
		Config:  config.DeviceConfig{ConnectHoldRF: true},
	}

	p.holdRadioOffOnConnect(w, "connect_hold_rf")

	if len(stub.setOpModeCalls) != 1 || stub.setOpModeCalls[0] != backend.ModeRFOff {
		t.Fatalf("连接期 LowPower 必须升级成持久 RFOff: %+v", stub.setOpModeCalls)
	}
}

// 幂等：已在飞行模式时不重复下发 SetOperatingMode。
func TestProjectionEnterAirplaneIdempotent(t *testing.T) {
	p := &Pool{ctx: context.Background()}
	stub := &workerStatusBackendStub{opMode: backend.ModeRFOff}
	w := &Worker{ID: "wwan0", Backend: stub}

	p.enterAirplaneModeFromPolicy(w, "test")

	if len(stub.setOpModeCalls) != 0 {
		t.Fatalf("已在飞行不应重复切: %+v", stub.setOpModeCalls)
	}
}

// 投影时按策略退出飞行：当前 RFOff 且策略不要求飞行 ⇒ 切回 Online。
func TestProjectionExitsAirplaneMode(t *testing.T) {
	p := &Pool{ctx: context.Background()}
	stub := &workerStatusBackendStub{opMode: backend.ModeRFOff}
	w := &Worker{ID: "wwan0", Backend: stub}

	p.exitAirplaneModeIfNeeded(w, "test")

	if len(stub.setOpModeCalls) != 1 || stub.setOpModeCalls[0] != backend.ModeOnline {
		t.Fatalf("应切回 Online: %+v", stub.setOpModeCalls)
	}
}

// 已在线时退出飞行是 no-op。
func TestProjectionExitAirplaneSkipsWhenOnline(t *testing.T) {
	p := &Pool{ctx: context.Background()}
	stub := &workerStatusBackendStub{opMode: backend.ModeOnline}
	w := &Worker{ID: "wwan0", Backend: stub}

	p.exitAirplaneModeIfNeeded(w, "test")

	if len(stub.setOpModeCalls) != 0 {
		t.Fatalf("已在线不应切: %+v", stub.setOpModeCalls)
	}
}

type stubPolicyResolver struct {
	pol cardpolicy.Policy
	err error
}

func (s *stubPolicyResolver) Resolve(iccid string) (cardpolicy.Policy, error) {
	return s.pol, s.err
}

func TestResolveAndApplyPolicy_EmptyICCID(t *testing.T) {
	p := &Pool{}
	w := &Worker{ID: "wwan0"}
	p.SetPolicyResolver(&stubPolicyResolver{})

	res := p.resolveAndApplyPolicy(w, "test")
	if res.Applied || res.Reason != "iccid_empty" {
		t.Fatalf("空 ICCID 应返回 iccid_empty: %+v", res)
	}
}

func TestResolveAndApplyPolicyNetworkOffKeepsCamped(t *testing.T) {
	p := &Pool{ctx: context.Background()}
	p.SetPolicyResolver(&stubPolicyResolver{
		pol: cardpolicy.Policy{ICCID: "123", VoWiFiEnabled: false, AirplaneEnabled: false, NetworkEnabled: false},
	})
	stub := &workerStatusBackendStub{opMode: backend.ModeOnline}
	w := &Worker{ID: "wwan0", Backend: stub}
	w.state.Identity.ICCID = "123"

	p.workers = map[string]*Worker{w.ID: w}
	res := p.resolveAndApplyPolicy(w, "vowifi_disabled")
	if !res.Applied {
		t.Fatalf("应成功应用: %+v", res)
	}
	for _, mode := range stub.setOpModeCalls {
		if mode == backend.ModeRFOff {
			t.Fatalf("网络关闭应保持驻网，不应切入飞行: %+v", stub.setOpModeCalls)
		}
	}
	if w.cellularRadioIsSuppressed() {
		t.Fatal("网络关闭仍应允许驻网")
	}
}

func TestResolveAndApplyPolicy_ResolvesAndProjects(t *testing.T) {
	p := &Pool{ctx: context.Background()}
	p.SetPolicyResolver(&stubPolicyResolver{
		pol: cardpolicy.Policy{ICCID: "123", AirplaneEnabled: true},
	})
	stub := &workerStatusBackendStub{opMode: backend.ModeOnline}
	w := &Worker{ID: "wwan0", Backend: stub}
	w.state.Identity.ICCID = "123"

	p.workers = map[string]*Worker{w.ID: w}
	res := p.resolveAndApplyPolicy(w, "test")
	if !res.Applied {
		t.Fatalf("应成功应用: %+v", res)
	}
	if !w.Config.AirplaneEnabled {
		t.Fatal("策略投影失败")
	}
	if len(stub.setOpModeCalls) != 1 || stub.setOpModeCalls[0] != backend.ModeRFOff {
		t.Fatalf("应切入飞行模式: %+v", stub.setOpModeCalls)
	}
}

func TestResolveAndApplyPolicyDoesNotRecoverVoWiFiWhenCardPolicyDisabled(t *testing.T) {
	p := NewPool(nil)
	defer p.cancel()
	p.SetPolicyResolver(&stubPolicyResolver{
		pol: cardpolicy.Policy{ICCID: "123", VoWiFiEnabled: false},
	})
	commands := make(chan vowifihost.LifecycleCommand, 1)
	p.voWiFiHost().LifecycleControllerForTest().TestRun = func(ctx context.Context, cmd vowifihost.LifecycleCommand) error {
		commands <- cmd
		return nil
	}
	w := &Worker{ID: "wwan0"}
	w.state.Identity.ICCID = "123"
	w.state.Identity.IMSI = "001010000000001"

	p.workers = map[string]*Worker{w.ID: w}
	res := p.resolveAndApplyPolicy(w, "test")
	if !res.Applied {
		t.Fatalf("应成功应用: %+v", res)
	}

	select {
	case cmd := <-commands:
		t.Fatalf("卡策略未开启 VoWiFi 时不应调度恢复: %+v", cmd)
	case <-time.After(120 * time.Millisecond):
	}
}

func TestResolveAndApplyPolicyDoesNotRecoverCellularOnDemand(t *testing.T) {
	p := NewPool(nil)
	defer p.cancel()
	p.SetPolicyResolver(&stubPolicyResolver{
		pol: cardpolicy.Policy{
			ICCID:         "123",
			VoWiFiEnabled: true,
			PhoneMode:     "cellular",
			DataStrategy:  "on_demand",
		},
	})
	commands := make(chan vowifihost.LifecycleCommand, 1)
	p.voWiFiHost().LifecycleControllerForTest().TestRun = func(ctx context.Context, cmd vowifihost.LifecycleCommand) error {
		commands <- cmd
		return nil
	}
	w := &Worker{ID: "wwan0"}
	w.state.Identity.ICCID = "123"
	w.state.Identity.IMSI = "001010000000001"

	p.workers = map[string]*Worker{w.ID: w}
	res := p.resolveAndApplyPolicy(w, "startup_post_apply")
	if !res.Applied {
		t.Fatalf("应成功应用: %+v", res)
	}
	if w.Config.PhoneMode != "cellular" || w.Config.AirplaneEnabled {
		t.Fatalf("蜂窝 on_demand 未开流量应保持驻网: %+v", w.Config)
	}
	if w.cellularRadioIsSuppressed() {
		t.Fatal("蜂窝 on_demand 未开流量仍应允许驻网")
	}

	select {
	case cmd := <-commands:
		t.Fatalf("蜂窝 on_demand 待机不应调度 VoWiFi 恢复: %+v", cmd)
	case <-time.After(120 * time.Millisecond):
	}
}

func TestResolveAndApplyPolicyDoesNotRecoverCellularAirplane(t *testing.T) {
	p := NewPool(nil)
	defer p.cancel()
	p.SetPolicyResolver(&stubPolicyResolver{
		pol: cardpolicy.Policy{
			ICCID:           "123",
			VoWiFiEnabled:   true,
			PhoneMode:       "cellular",
			DataStrategy:    "always",
			AirplaneEnabled: true,
			NetworkEnabled:  true,
		},
	})
	commands := make(chan vowifihost.LifecycleCommand, 1)
	p.voWiFiHost().LifecycleControllerForTest().TestRun = func(ctx context.Context, cmd vowifihost.LifecycleCommand) error {
		commands <- cmd
		return nil
	}
	w := &Worker{ID: "wwan0"}
	w.state.Identity.ICCID = "123"
	w.state.Identity.IMSI = "001010000000001"

	p.workers = map[string]*Worker{w.ID: w}
	res := p.resolveAndApplyPolicy(w, "flight_mode_change")
	if !res.Applied {
		t.Fatalf("应成功应用: %+v", res)
	}
	if !w.Config.AirplaneEnabled || w.Config.NetworkEnabled || !w.Config.VoWiFiEnabled {
		t.Fatalf("蜂窝飞行应保留软件电话并关流量: %+v", w.Config)
	}

	select {
	case cmd := <-commands:
		t.Fatalf("蜂窝飞行不应调度 VoWiFi 恢复: %+v", cmd)
	case <-time.After(120 * time.Millisecond):
	}
}

func TestRefreshIdentityAndApplyCardPolicyDoesNotFallbackToConfiguredNetworkWithoutResolver(t *testing.T) {
	p := NewPool(nil)
	defer p.cancel()

	ctrl := &fakeController{}
	w := &Worker{
		ID: "wwan0",
		Config: config.DeviceConfig{
			ID:             "wwan0",
			NetworkEnabled: true,
		},
		Backend: &workerStartupIdentityBackendStub{
			liveICCID: "898600000000000001",
			liveIMSI:  "460001234567890",
		},
		netOverride: ctrl,
	}

	result, err := p.refreshIdentityAndApplyCardPolicy(w, "startup_post_apply")
	if err != nil {
		t.Fatalf("refreshIdentityAndApplyCardPolicy() error=%v", err)
	}
	if result.ICCID != "898600000000000001" || result.IMSI != "460001234567890" {
		t.Fatalf("live identity result mismatch: %+v", result)
	}
	if ctrl.connected {
		t.Fatal("无 policy resolver 时不应回退到旧 worker.Config 连接数据网络")
	}
}

func TestRefreshIdentityAndApplyCardPolicyReturnsResolverError(t *testing.T) {
	expected := errors.New("temporary policy store failure")
	p := NewPool(nil)
	defer p.cancel()
	p.SetPolicyResolver(&stubPolicyResolver{err: expected})
	w := &Worker{
		ID: "wwan0",
		Backend: &workerStartupIdentityBackendStub{
			liveICCID: "898600000000000001",
			liveIMSI:  "460001234567890",
		},
	}

	p.workers = map[string]*Worker{w.ID: w}
	_, err := p.refreshIdentityAndApplyCardPolicy(w, "startup_post_apply")
	if !errors.Is(err, expected) {
		t.Fatalf("refresh error = %v, want wrapped resolver error", err)
	}
}

// hideck#94: QMI DMS hung for two hours while the AT port kept answering.
func radioOffATTestPool(t *testing.T, backendMode string, cfunReply string) (*Pool, *Worker, *[]string) {
	t.Helper()
	p, w := atTestPool(t)
	w.Backend = &workerStatusBackendStub{
		mode: backendMode, opMode: backend.ModeOnline,
		setOpModeErr: errors.New("QMI 服务未就绪: DMS"),
	}
	commands := &[]string{}
	p.openATSession = func(string) (atSerialSession, error) {
		return &controlledATSession{execute: func(cmd string, _ time.Duration) (string, error) {
			*commands = append(*commands, cmd)
			if cmd == "AT+CFUN?" {
				return cfunReply, nil
			}
			return "OK", nil
		}}, nil
	}
	return p, w, commands
}

func TestAirplaneFallsBackToATWhenQMIFails(t *testing.T) {
	p, w, commands := radioOffATTestPool(t, backend.BackendQMI, "+CFUN: 4\r\nOK")
	p.enterAirplaneModeFromPolicy(w, "test")
	if got := strings.Join(*commands, ","); got != "AT+CFUN=4,AT+CFUN?" {
		t.Fatalf("AT commands = %s", got)
	}
	if !p.radioOffOverAT(w, "test", errors.New("QMI 服务未就绪: DMS")) {
		t.Fatal("confirmed CFUN 4 was reported as a failure")
	}
}

func TestAirplaneATFallbackRequiresReadback(t *testing.T) {
	p, w, _ := radioOffATTestPool(t, backend.BackendQMI, "+CFUN: 1\r\nOK")
	if p.radioOffOverAT(w, "test", errors.New("QMI 服务未就绪: DMS")) {
		t.Fatal("AT fallback trusted CFUN=4 without reading back 4")
	}
}

func TestConnectHoldFallsBackToATWhenQMIFails(t *testing.T) {
	p, w, commands := radioOffATTestPool(t, backend.BackendQMI, "+CFUN: 4\r\nOK")
	w.Config.ConnectHoldRF = true
	p.holdRadioOffOnConnect(w, "connect_hold_rf")
	if got := strings.Join(*commands, ","); got != "AT+CFUN=4,AT+CFUN?" {
		t.Fatalf("AT commands = %s", got)
	}
	// CFUN=4 may not survive a restart, so the hold stays for QMI to redo in PersistLow.
	if !w.Config.ConnectHoldRF {
		t.Fatal("an AT radio-off cleared ConnectHoldRF")
	}
}

func TestConnectHoldKeptWhenATReadbackFails(t *testing.T) {
	p, w, _ := radioOffATTestPool(t, backend.BackendQMI, "+CFUN: 1\r\nOK")
	w.Config.ConnectHoldRF = true
	p.holdRadioOffOnConnect(w, "connect_hold_rf")
	if !w.Config.ConnectHoldRF {
		t.Fatal("a failed radio-off cleared ConnectHoldRF")
	}
}

func TestAirplaneATFallbackSkipsExplicitQMIFailure(t *testing.T) {
	p, w, commands := radioOffATTestPool(t, backend.BackendQMI, "+CFUN: 4\r\nOK")
	w.Backend.(*workerStatusBackendStub).setOpModeErr = errors.New("DMS SetOperatingMode: QMI_ERR_INVALID_TRANSITION")
	p.enterAirplaneModeFromPolicy(w, "test")
	if len(*commands) != 0 {
		t.Fatalf("an explicit QMI failure was retried over AT: %v", *commands)
	}
}

func TestAirplaneATFallbackStopsWhenSetFails(t *testing.T) {
	p, w, commands := radioOffATTestPool(t, backend.BackendQMI, "+CFUN: 4\r\nOK")
	p.openATSession = func(string) (atSerialSession, error) {
		return &controlledATSession{execute: func(cmd string, _ time.Duration) (string, error) {
			*commands = append(*commands, cmd)
			return "ERROR", errors.New("AT 命令返回 ERROR")
		}}, nil
	}
	if p.radioOffOverAT(w, "test", errors.New("QMI 服务未就绪: DMS")) {
		t.Fatal("a failed AT+CFUN=4 was reported as success")
	}
	if got := strings.Join(*commands, ","); got != "AT+CFUN=4" {
		t.Fatalf("AT commands = %s", got)
	}
}

func TestAirplaneATFallbackStopsOnATError(t *testing.T) {
	logger.Setup(logger.LogConfig{Debug: true, Filename: filepath.Join(t.TempDir(), "app.log")})
	logs := logger.GlobalBroadcaster.Subscribe()
	defer logger.GlobalBroadcaster.Unsubscribe(logs)
	for _, tc := range []struct {
		reply   string
		err     error
		message string
	}{
		// SerialAT returns as soon as it sees ERROR, with a nil error.
		{"ERROR\r\n", nil, "AT+CFUN=4 没有返回 OK"},
		// +CME ERROR does not end its read early: it times out with an error.
		{"+CME ERROR: 3\r\n", errors.New("timeout"), "AT+CFUN=4 也失败"},
	} {
		p, w, commands := radioOffATTestPool(t, backend.BackendQMI, "+CFUN: 4\r\nOK")
		p.openATSession = func(string) (atSerialSession, error) {
			return &controlledATSession{execute: func(cmd string, _ time.Duration) (string, error) {
				*commands = append(*commands, cmd)
				if cmd == "AT+CFUN?" {
					return "+CFUN: 4\r\nOK\r\n", nil
				}
				return tc.reply, tc.err
			}}, nil
		}
		if p.radioOffOverAT(w, "test", errors.New("QMI 服务未就绪: DMS")) {
			t.Fatalf("AT+CFUN=4 answered %q was reported as success", tc.reply)
		}
		if got := strings.Join(*commands, ","); got != "AT+CFUN=4" {
			t.Fatalf("AT commands after %q = %s", tc.reply, got)
		}
		entry := waitLogEntry(t, logs, func(entry logger.LogEntry) bool {
			return strings.Contains(entry.Message, tc.message)
		})
		fields := readLogFields(t, entry)
		if fields["set_response"] != strings.TrimSpace(tc.reply) || fields["qmi_err"] == nil {
			t.Fatalf("log fields after %q = %v", tc.reply, fields)
		}
	}
}

func TestAirplaneATFallbackAcceptsOKBeforeAURC(t *testing.T) {
	p, w, commands := radioOffATTestPool(t, backend.BackendQMI, "+CFUN: 4\r\nOK")
	p.openATSession = func(string) (atSerialSession, error) {
		return &controlledATSession{execute: func(cmd string, _ time.Duration) (string, error) {
			*commands = append(*commands, cmd)
			if cmd == "AT+CFUN?" {
				return "+CFUN: 4\r\nOK\r\n", nil
			}
			return "AT+CFUN=4\r\r\nOK\r\n\r\n+CREG: 0\r\n", nil
		}}, nil
	}
	if !p.radioOffOverAT(w, "test", errors.New("QMI 服务未就绪: DMS")) {
		t.Fatalf("an OK followed by a URC was reported as a failure: %v", *commands)
	}
}

func TestAirplaneATFallbackFailsWhenQueryFails(t *testing.T) {
	logger.Setup(logger.LogConfig{Debug: true, Filename: filepath.Join(t.TempDir(), "app.log")})
	logs := logger.GlobalBroadcaster.Subscribe()
	defer logger.GlobalBroadcaster.Unsubscribe(logs)
	p, w, commands := radioOffATTestPool(t, backend.BackendQMI, "")
	p.openATSession = func(string) (atSerialSession, error) {
		return &controlledATSession{execute: func(cmd string, _ time.Duration) (string, error) {
			*commands = append(*commands, cmd)
			if cmd == "AT+CFUN?" {
				return "+CF", errors.New("timeout")
			}
			return "OK\r\n", nil
		}}, nil
	}
	if p.radioOffOverAT(w, "test", errors.New("QMI 服务未就绪: DMS")) {
		t.Fatal("a failed AT+CFUN? was reported as success")
	}
	if got := strings.Join(*commands, ","); got != "AT+CFUN=4,AT+CFUN?" {
		t.Fatalf("AT commands = %s", got)
	}
	entry := waitLogEntry(t, logs, func(entry logger.LogEntry) bool {
		return strings.Contains(entry.Message, "读回 AT+CFUN? 失败")
	})
	if fields := readLogFields(t, entry); fields["set_response"] != "OK" || fields["query_response"] != "+CF" ||
		fields["at_err"] == nil {
		t.Fatalf("log fields = %v", fields)
	}
}

func TestAirplaneATFallbackSkippedDuringShutdown(t *testing.T) {
	logger.Setup(logger.LogConfig{Debug: true, Filename: filepath.Join(t.TempDir(), "app.log")})
	logs := logger.GlobalBroadcaster.Subscribe()
	defer logger.GlobalBroadcaster.Unsubscribe(logs)
	p, w, commands := radioOffATTestPool(t, backend.BackendQMI, "+CFUN: 4\r\nOK")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.ctx = ctx
	if p.radioOffOverAT(w, "test", errors.New("context canceled")) || len(*commands) != 0 {
		t.Fatalf("fallback ran during shutdown: %v", *commands)
	}
	for drained := false; !drained; {
		select {
		case entry := <-logs:
			if strings.Contains(entry.Message, "明确失败") {
				t.Fatalf("shutdown was logged as an explicit QMI failure: %s", entry.Message)
			}
		default:
			drained = true
		}
	}
}

func TestAirplaneATFallbackFailsWhenThePortWillNotOpen(t *testing.T) {
	p, w, _ := radioOffATTestPool(t, backend.BackendQMI, "+CFUN: 4\r\nOK")
	p.openATSession = func(string) (atSerialSession, error) { return nil, errors.New("device busy") }
	if p.radioOffOverAT(w, "test", errors.New("QMI 服务未就绪: DMS")) {
		t.Fatal("an AT port that would not open was reported as success")
	}
}

func TestAirplaneATFallbackWaitsForTheATPortOnlyBriefly(t *testing.T) {
	p, w, commands := radioOffATTestPool(t, backend.BackendQMI, "+CFUN: 4\r\nOK")
	release, err := p.acquireDeviceAT(context.Background(), w.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	started := time.Now()
	if p.radioOffOverAT(w, "test", errors.New("QMI 服务未就绪: DMS")) || len(*commands) != 0 {
		t.Fatalf("fallback ran while the AT port was busy: %v", *commands)
	}
	if waited := time.Since(started); waited < radioOffATGateWait || waited > radioOffATGateWait+2*time.Second {
		t.Fatalf("waited %s for the AT port, want about %s", waited, radioOffATGateWait)
	}
}

func TestAirplaneATFallbackOnlyForQMIBackend(t *testing.T) {
	p, w, commands := radioOffATTestPool(t, backend.BackendAT, "+CFUN: 4\r\nOK")
	p.enterAirplaneModeFromPolicy(w, "test")
	if len(*commands) != 0 {
		t.Fatalf("AT backend failure was retried over AT: %v", *commands)
	}
}

func TestAirplaneATFallbackSkipsAReplacedWorker(t *testing.T) {
	p, old, commands := radioOffATTestPool(t, backend.BackendQMI, "+CFUN: 4\r\nOK")
	replacement := &Worker{ID: old.ID, Config: old.Config, Backend: old.Backend}
	p.workers[old.ID] = replacement
	if p.radioOffOverAT(old, "test", errors.New("QMI 服务未就绪: DMS")) || len(*commands) != 0 {
		t.Fatalf("a replaced Worker's fallback reached the new Worker: %v", *commands)
	}
}

func TestATFinalOK(t *testing.T) {
	for response, want := range map[string]bool{
		"OK\r\n":                        true,
		"\r\nOK\r\n":                    true,
		"AT+CFUN=4\r\r\nOK\r\n":         true,
		"\r\n\r\nOK":                    true,
		"OK\r\n\r\n+CREG: 0\r\n":        true,
		"ERROR\r\n":                     false,
		"+CME ERROR: 3\r\n":             false,
		"+CMS ERROR: 500\r\nOK\r\n":     false,
		"+CME ERROR_COUNT: 0\r\nOK\r\n": true,
		"+CREG: 0\r\nERROR\r\nOK\r\n":   false,
		"":                              false,
		"+CFUN: 4\r\n":                  false,
	} {
		if got := atFinalOK(response); got != want {
			t.Errorf("atFinalOK(%q) = %v, want %v", response, got, want)
		}
	}
}

func TestParseCFUN(t *testing.T) {
	for _, tc := range []struct {
		response string
		fun      int
		ok       bool
	}{
		{"+CFUN: 4\r\nOK", 4, true},
		{"+CFUN:4", 4, true},
		{"\r\n+CFUN: 40\r\n\r\nOK\r\n", 40, true},
		{"+CFUN: 1,0", 0, false},
		{"AT+CFUN?\r\r\n+CREG: 0\r\n+CFUN: 4\r\n\r\nOK\r\n", 4, true},
		{"OK", 0, false},
	} {
		fun, ok := parseCFUN(tc.response)
		if fun != tc.fun || ok != tc.ok {
			t.Errorf("parseCFUN(%q) = %d,%v want %d,%v", tc.response, fun, ok, tc.fun, tc.ok)
		}
	}
}
