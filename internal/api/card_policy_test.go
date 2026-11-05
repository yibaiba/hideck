package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/gin-gonic/gin"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/db"
	"github.com/yibaiba/hideck/internal/device"
)

type cardPolicyStoreStub struct {
	policy     db.CardPolicy
	getErr     error
	resolveErr error
	upsertErr  error
	cleared    []string
	remembered bool
	clearErr   error
}

func (s *cardPolicyStoreStub) ClearMWIRejections(iccid string) (bool, error) {
	s.cleared = append(s.cleared, iccid)
	return s.remembered, s.clearErr
}

type cardPolicyRestarterStub struct {
	iccid string
	err   error
}

func (s *cardPolicyRestarterStub) RestartVoWiFiForICCID(iccid string) error {
	s.iccid = iccid
	return s.err
}

func (s *cardPolicyStoreStub) Get(string) (db.CardPolicy, error) {
	return s.policy, s.getErr
}

func (s *cardPolicyStoreStub) Resolve(string) (db.CardPolicy, error) {
	return s.policy, s.resolveErr
}

func (s *cardPolicyStoreStub) Upsert(policy db.CardPolicy) error {
	s.policy = policy
	return s.upsertErr
}

// injectWorker 通过 unsafe 反射将 worker 注入到 pool 的内部 workers map，
// 用于无需完整启动流程的测试场景。
func injectWorker(p *device.Pool, w *device.Worker) {
	pv := reflect.ValueOf(p).Elem().FieldByName("workers")
	m := reflect.NewAt(pv.Type(), unsafe.Pointer(pv.UnsafeAddr())).Elem()
	m.SetMapIndex(reflect.ValueOf(w.ID), reflect.ValueOf(w))
}

func openTestDB(t *testing.T) {
	t.Helper()
	if err := db.Init(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("Init() error=%v", err)
	}
	t.Cleanup(func() {
		if db.DB != nil {
			if sqlDB, err := db.DB.DB(); err == nil && sqlDB != nil {
				_ = sqlDB.Close()
			}
		}
		db.DB = nil
	})
}

func TestGetCardPolicyEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openTestDB(t)
	_ = db.UpsertCardPolicy(db.CardPolicy{ICCID: "8986004", NetworkEnabled: true, IPVersion: "v4", Source: "user"})

	s := &Server{}
	r := gin.Default()
	r.GET("/api/cards/:iccid/policy", s.handleGetCardPolicy)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cards/8986004/policy", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var got db.CardPolicy
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.NetworkEnabled {
		t.Fatalf("payload 错: %+v", got)
	}
}

func TestPutCardPolicyEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openTestDB(t)
	s := &Server{
		pool: device.NewPool(&config.Config{}),
	}
	r := gin.Default()
	r.PUT("/api/cards/:iccid/policy", s.handlePutCardPolicy)

	body := `{"network_enabled":true,"vowifi_enabled":true,"ip_version":"v4v6","apn":"ims"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/cards/8986005/policy", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	got, _ := db.GetCardPolicy("8986005")
	if !got.NetworkEnabled || !got.VoWiFiEnabled || got.IPVersion != "v4v6" || got.APN != "ims" {
		t.Fatalf("未成功更新: %+v", got)
	}
}

func TestPutCardPolicyCanClearAPN(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &cardPolicyStoreStub{policy: db.CardPolicy{
		ICCID: "8986005", IPVersion: "v4v6", APN: "ims", Source: "user",
	}}
	s := &Server{cardPolicies: store}
	r := gin.New()
	r.PUT("/api/cards/:iccid/policy", s.handlePutCardPolicy)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/cards/8986005/policy", strings.NewReader(`{"apn":""}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK || store.policy.APN != "" {
		t.Fatalf("code=%d apn=%q body=%s", w.Code, store.policy.APN, w.Body.String())
	}
	if store.policy.IPVersion != "v4v6" {
		t.Fatalf("omitted ip_version changed to %q", store.policy.IPVersion)
	}
}

func TestPutCardPolicySavesAndClearsManualPhoneWithoutRestart(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &cardPolicyStoreStub{policy: db.CardPolicy{ICCID: "8986005", IPVersion: "v4"}}
	restarter := &cardPolicyRestarterStub{}
	s := &Server{cardPolicies: store, cardPolicyRestarter: restarter}
	r := gin.New()
	r.PUT("/api/cards/:iccid/policy", s.handlePutCardPolicy)
	put := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/cards/8986005/policy", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}

	if w := put(`{"local_phone_override":"  +447911123456  "}`); w.Code != http.StatusOK {
		t.Fatalf("save code=%d response=%s", w.Code, w.Body.String())
	}
	if store.policy.LocalPhoneOverride != "+447911123456" {
		t.Fatalf("manual number=%q", store.policy.LocalPhoneOverride)
	}
	if w := put(`{"local_phone_override":""}`); w.Code != http.StatusOK {
		t.Fatalf("clear code=%d response=%s", w.Code, w.Body.String())
	}
	if store.policy.LocalPhoneOverride != "" {
		t.Fatalf("manual number was not cleared: %+v", store.policy)
	}
	if restarter.iccid != "" {
		t.Fatalf("manual number restarted VoWiFi for %q", restarter.iccid)
	}
}

func TestPutCardPolicyRejectsInvalidManualPhone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &cardPolicyStoreStub{policy: db.CardPolicy{ICCID: "8986005", IPVersion: "v4"}}
	s := &Server{cardPolicies: store}
	r := gin.New()
	r.PUT("/api/cards/:iccid/policy", s.handlePutCardPolicy)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/cards/8986005/policy", strings.NewReader(`{"local_phone_override":"+44 7911"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || store.policy.LocalPhoneOverride != "" {
		t.Fatalf("code=%d policy=%+v body=%s", w.Code, store.policy, w.Body.String())
	}
}

func TestPutCardPolicyVoWiFiUpstreamProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openTestDB(t)
	now := time.Now()
	if err := db.UpsertUpstreamProxy(db.UpstreamProxy{ID: "uk-a", Addr: "127.0.0.1:1080", Enabled: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	s := &Server{pool: device.NewPool(&config.Config{})}
	r := gin.New()
	r.PUT("/api/cards/:iccid/policy", s.handlePutCardPolicy)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/cards/8944101/policy", strings.NewReader(`{"vowifi_upstream_proxy_id":"uk-a"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	got, err := db.GetCardPolicy("8944101")
	if err != nil || got.VowifiUpstreamProxyID != "uk-a" {
		t.Fatalf("got=%+v err=%v", got, err)
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/cards/8944101/policy", strings.NewReader(`{"vowifi_upstream_proxy_id":"missing"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing proxy code=%d body=%s", w.Code, w.Body.String())
	}

	if err := db.UpsertUpstreamProxy(db.UpstreamProxy{
		ID: "uk-disabled", Addr: "127.0.0.1:1081", Enabled: false, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/cards/8944101/policy", strings.NewReader(
		`{"vowifi_upstream_proxy_id":"uk-disabled"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("disabled proxy code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestPutCardPolicyReportsSavedPolicyWhenVoWiFiRestartFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openTestDB(t)
	now := time.Now()
	if err := db.UpsertUpstreamProxy(db.UpstreamProxy{
		ID: "uk-a", Addr: "127.0.0.1:1080", Enabled: true, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	store := &cardPolicyStoreStub{policy: db.CardPolicy{
		ICCID: "8944101", VoWiFiEnabled: true, Source: "user",
	}}
	restarter := &cardPolicyRestarterStub{err: errors.New("restart unavailable")}
	s := &Server{cardPolicies: store, cardPolicyRestarter: restarter}
	r := gin.New()
	r.PUT("/api/cards/:iccid/policy", s.handlePutCardPolicy)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/cards/8944101/policy", strings.NewReader(
		`{"vowifi_upstream_proxy_id":"uk-a"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if store.policy.VowifiUpstreamProxyID != "uk-a" || restarter.iccid != "8944101" {
		t.Fatalf("policy=%+v restarted_iccid=%q", store.policy, restarter.iccid)
	}
	var body struct {
		Code   string        `json:"code"`
		Policy db.CardPolicy `json:"policy"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != cardPolicySavedRestartFailedCode || body.Policy.VowifiUpstreamProxyID != "uk-a" {
		t.Fatalf("response=%+v", body)
	}
}

func TestPutCardPolicyRejectsInvalidIPVersion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &cardPolicyStoreStub{policy: db.CardPolicy{ICCID: "8986005", IPVersion: "v4"}}
	s := &Server{cardPolicies: store}
	r := gin.New()
	r.PUT("/api/cards/:iccid/policy", s.handlePutCardPolicy)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/cards/8986005/policy", strings.NewReader(`{"ip_version":"v9"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest || store.policy.IPVersion != "v4" {
		t.Fatalf("code=%d policy=%+v body=%s", w.Code, store.policy, w.Body.String())
	}
}

func TestPutCardPolicyDoesNotHideReadFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &cardPolicyStoreStub{getErr: errors.New("database unavailable")}
	s := &Server{cardPolicies: store}
	r := gin.New()
	r.PUT("/api/cards/:iccid/policy", s.handlePutCardPolicy)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/cards/8986005/policy", strings.NewReader(`{"network_enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "database unavailable") {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

// TestPatchCardPolicyForDevice 验证 patchCardPolicyForDevice helper 正确解析 ICCID 并落库。
func TestPatchCardPolicyForDevice(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openTestDB(t)

	p := device.NewPool(&config.Config{})
	w := &device.Worker{ID: "wwan-patch"}
	setNestedPrivateField(t, w, []string{"state", "Identity", "ICCID"}, "8986patch001")
	injectWorker(p, w)

	s := &Server{pool: p}
	iccid, applied, err := s.patchCardPolicyForDevice("wwan-patch", func(pol *db.CardPolicy) {
		pol.NetworkEnabled = true
		pol.IPVersion = "v4v6"
		pol.APN = "ims"
	})

	if err != nil {
		t.Fatalf("error=%v", err)
	}
	if !applied {
		t.Fatalf("expected applied=true")
	}
	if iccid != "8986patch001" {
		t.Fatalf("iccid=%q", iccid)
	}
	got, err := db.GetCardPolicy("8986patch001")
	if err != nil {
		t.Fatal(err)
	}
	if !got.NetworkEnabled || got.IPVersion != "v4v6" || got.APN != "ims" {
		t.Fatalf("card policy mismatch: %+v", got)
	}
	if got.Source != "user" {
		t.Fatalf("source=%q want user", got.Source)
	}
}

// TestPatchCardPolicyForDeviceNoICCID 验证设备无 ICCID 时 applied=false 且不报错。
func TestPatchCardPolicyForDeviceNoICCID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openTestDB(t)

	p := device.NewPool(&config.Config{})
	w := &device.Worker{ID: "wwan-nocard"}
	// 不设置 ICCID，模拟无卡状态
	injectWorker(p, w)

	s := &Server{pool: p}
	iccid, applied, err := s.patchCardPolicyForDevice("wwan-nocard", func(pol *db.CardPolicy) {
		pol.NetworkEnabled = true
	})

	if !errors.Is(err, errCardPolicyIdentityUnavailable) {
		t.Fatalf("error=%v", err)
	}
	if applied {
		t.Fatalf("expected applied=false when no ICCID")
	}
	if iccid != "" {
		t.Fatalf("iccid=%q want empty", iccid)
	}
}

func TestNetworkPatchStopsBeforeRuntimeMutationWhenPolicyWriteFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	p := device.NewPool(&config.Config{})
	w := &device.Worker{ID: "wwan-fail", Config: config.DeviceConfig{ID: "wwan-fail", NetworkEnabled: true}}
	setNestedPrivateField(t, w, []string{"state", "Identity", "ICCID"}, "8986fail001")
	injectWorker(p, w)
	s := &Server{
		pool: p,
		cardPolicies: &cardPolicyStoreStub{
			policy:    db.CardPolicy{ICCID: "8986fail001", NetworkEnabled: true},
			upsertErr: errors.New("write failed"),
		},
	}
	r := gin.New()
	r.PATCH("/api/devices/:device_id/network", s.handleDeviceNetworkPatch)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPatch, "/api/devices/wwan-fail/network", strings.NewReader(`{"enabled":false}`))
	request.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !w.Config.NetworkEnabled {
		t.Fatal("runtime policy changed after persistence failure")
	}
}

// TestPatchCardPolicyVoWiFiKeepsAirplaneIntent 验证开 VoWiFi 不再强制 airplane=true：
// airplane 反映用户的纯飞行意图，独立于 vowifi。
func TestPatchCardPolicyVoWiFiKeepsAirplaneIntent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openTestDB(t)

	p := device.NewPool(&config.Config{})
	w := &device.Worker{ID: "wwan-vowifi"}
	setNestedPrivateField(t, w, []string{"state", "Identity", "ICCID"}, "8986vowifi01")
	injectWorker(p, w)

	s := &Server{pool: p}
	if err := db.UpsertCardPolicy(db.CardPolicy{
		ICCID: "8986vowifi01", NetworkEnabled: false, VoWiFiEnabled: false, AirplaneEnabled: false, IPVersion: "v4", Source: "user",
	}); err != nil {
		t.Fatal(err)
	}
	// 从在线开 VoWiFi（飞行意图为 false）：airplane 应保持 false，不被强制为 true。
	_, _, err := s.patchCardPolicyForDevice("wwan-vowifi", vowifiEnablePolicyMutation)
	if err != nil {
		t.Fatalf("error=%v", err)
	}
	got, _ := db.GetCardPolicy("8986vowifi01")
	if !got.VoWiFiEnabled || got.AirplaneEnabled {
		t.Fatalf("开 VoWiFi 不应强制 airplane=true: vowifi=%v airplane=%v", got.VoWiFiEnabled, got.AirplaneEnabled)
	}
}

// TestVoWiFiToggleCyclePreservesAirplaneIntent 复现并锁定 bug 修复：
// 先开飞行 → 开 VoWiFi → 关 VoWiFi，应回退到飞行（airplane 意图被保留）。
func TestVoWiFiToggleCyclePreservesAirplaneIntent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openTestDB(t)

	p := device.NewPool(&config.Config{})
	w := &device.Worker{ID: "wwan-cycle"}
	setNestedPrivateField(t, w, []string{"state", "Identity", "ICCID"}, "8986cycle001")
	injectWorker(p, w)
	s := &Server{pool: p}

	// 1) 用户先开飞行
	if _, _, err := s.patchCardPolicyForDevice("wwan-cycle", func(pol *db.CardPolicy) {
		pol.AirplaneEnabled = true
		pol.VoWiFiEnabled = false
		pol.NetworkEnabled = false
	}); err != nil {
		t.Fatalf("开飞行 error=%v", err)
	}

	// 2) 开 VoWiFi（落库副作用：只置 vowifi）
	if _, _, err := s.patchCardPolicyForDevice("wwan-cycle", vowifiEnablePolicyMutation); err != nil {
		t.Fatalf("开 vowifi error=%v", err)
	}
	mid, _ := db.GetCardPolicy("8986cycle001")
	if !mid.VoWiFiEnabled || !mid.AirplaneEnabled {
		t.Fatalf("开 VoWiFi 期间飞行意图应保留: %+v", mid)
	}

	// 3) 关 VoWiFi（落库副作用：只清 vowifi），应回退到飞行
	if _, _, err := s.patchCardPolicyForDevice("wwan-cycle", vowifiDisablePolicyMutation); err != nil {
		t.Fatalf("关 vowifi error=%v", err)
	}
	got, _ := db.GetCardPolicy("8986cycle001")
	if got.VoWiFiEnabled || !got.AirplaneEnabled {
		t.Fatalf("关 VoWiFi 后应回退到飞行模式: vowifi=%v airplane=%v", got.VoWiFiEnabled, got.AirplaneEnabled)
	}
}

// TestPatchCardPolicyAirplaneMutualExclusion 验证“开飞行模式”落库时与 network/vowifi 互斥
// （等价于 handleDeviceMgmtSetFlightMode 开飞行时的落库副作用）。
func TestApplyAirplaneToCardPolicyInterlock(t *testing.T) {
	wifi := db.CardPolicy{PhoneMode: "wifi", VoWiFiEnabled: true, NetworkEnabled: true}
	applyAirplaneToCardPolicy(&wifi, true)
	if !wifi.AirplaneEnabled || wifi.NetworkEnabled || wifi.VoWiFiEnabled {
		t.Fatalf("WiFi calling 开飞行应关电话和流量: %+v", wifi)
	}

	cell := db.CardPolicy{PhoneMode: "cellular", VoWiFiEnabled: true, NetworkEnabled: true}
	applyAirplaneToCardPolicy(&cell, true)
	if !cell.AirplaneEnabled || cell.NetworkEnabled || !cell.VoWiFiEnabled {
		t.Fatalf("蜂窝开飞行应保留软件电话并关流量: %+v", cell)
	}

	applyAirplaneToCardPolicy(&cell, false)
	if cell.AirplaneEnabled || !cell.VoWiFiEnabled {
		t.Fatalf("关飞行只清 airplane: %+v", cell)
	}

	always := db.CardPolicy{PhoneMode: "cellular", VoWiFiEnabled: true, DataStrategy: "always", NetworkEnabled: false, AirplaneEnabled: true}
	applyAirplaneToCardPolicy(&always, false)
	if always.AirplaneEnabled || !always.NetworkEnabled || !always.VoWiFiEnabled {
		t.Fatalf("蜂窝 always 关飞行应写回网络: %+v", always)
	}
}

func TestApplyNetworkAndVoWiFiInterlock(t *testing.T) {
	wifi := db.CardPolicy{PhoneMode: "wifi", VoWiFiEnabled: true, AirplaneEnabled: true}
	applyNetworkEnableToCardPolicy(&wifi)
	if !wifi.NetworkEnabled || wifi.AirplaneEnabled || wifi.VoWiFiEnabled {
		t.Fatalf("开网络应驻网并关掉 WiFi calling: %+v", wifi)
	}

	cell := db.CardPolicy{PhoneMode: "cellular", VoWiFiEnabled: true, AirplaneEnabled: true, DataStrategy: "on_demand"}
	applyNetworkEnableToCardPolicy(&cell)
	if !cell.NetworkEnabled || cell.AirplaneEnabled || !cell.VoWiFiEnabled {
		t.Fatalf("蜂窝开网络应驻网并保留软件电话: %+v", cell)
	}

	wifiPhone := db.CardPolicy{PhoneMode: "wifi", NetworkEnabled: true}
	applyVoWiFiEnableToCardPolicy(&wifiPhone)
	if !wifiPhone.VoWiFiEnabled || !wifiPhone.AirplaneEnabled || wifiPhone.NetworkEnabled {
		t.Fatalf("开 WiFi calling 应锁定飞行并关流量: %+v", wifiPhone)
	}

	cellPhone := db.CardPolicy{PhoneMode: "cellular", DataStrategy: "on_demand", AirplaneEnabled: true}
	applyVoWiFiEnableToCardPolicy(&cellPhone)
	if !cellPhone.VoWiFiEnabled || cellPhone.AirplaneEnabled || cellPhone.NetworkEnabled {
		t.Fatalf("开蜂窝软件电话应驻网且默认不开流量: %+v", cellPhone)
	}

	voltePhone := db.CardPolicy{PhoneMode: "volte", AirplaneEnabled: true, NetworkEnabled: false}
	applyVoWiFiEnableToCardPolicy(&voltePhone)
	if !voltePhone.VoWiFiEnabled || voltePhone.AirplaneEnabled || voltePhone.NetworkEnabled {
		t.Fatalf("开 VoLTE 应驻网且不强制开上网流量: %+v", voltePhone)
	}

	applyAirplaneToCardPolicy(&voltePhone, true)
	if !voltePhone.AirplaneEnabled || voltePhone.NetworkEnabled || !voltePhone.VoWiFiEnabled {
		t.Fatalf("VoLTE 开飞行应保留电话并关流量: %+v", voltePhone)
	}
}

func TestPatchCardPolicyAirplaneMutualExclusion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openTestDB(t)

	// 预置：network 开着、vowifi 开着
	_ = db.UpsertCardPolicy(db.CardPolicy{ICCID: "8986air001", NetworkEnabled: true, VoWiFiEnabled: true, Source: "user"})

	p := device.NewPool(&config.Config{})
	w := &device.Worker{ID: "wwan-air"}
	setNestedPrivateField(t, w, []string{"state", "Identity", "ICCID"}, "8986air001")
	injectWorker(p, w)

	s := &Server{pool: p}
	// 开飞行：airplane=on，且互斥关 network/vowifi
	_, applied, err := s.patchCardPolicyForDevice("wwan-air", func(pol *db.CardPolicy) {
		pol.AirplaneEnabled = true
		pol.VoWiFiEnabled = false
		pol.NetworkEnabled = false
	})
	if err != nil || !applied {
		t.Fatalf("applied=%v err=%v", applied, err)
	}

	got, _ := db.GetCardPolicy("8986air001")
	if !got.AirplaneEnabled || got.NetworkEnabled || got.VoWiFiEnabled {
		t.Fatalf("开飞行应互斥关 network/vowifi: %+v", got)
	}
}

func TestPutCardPolicyAirplaneField(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openTestDB(t)
	// 预置一行 network=true，验证只 PUT airplane 时 network 不被指针语义覆盖
	_ = db.UpsertCardPolicy(db.CardPolicy{ICCID: "8986air777", NetworkEnabled: true, IPVersion: "v4", Source: "user"})

	s := &Server{pool: device.NewPool(&config.Config{})}
	r := gin.Default()
	r.PUT("/api/cards/:iccid/policy", s.handlePutCardPolicy)

	body := `{"airplane_enabled":true}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/cards/8986air777/policy", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	got, _ := db.GetCardPolicy("8986air777")
	if !got.AirplaneEnabled {
		t.Fatalf("airplane 未写入: %+v", got)
	}
	if !got.NetworkEnabled {
		t.Fatalf("未传的 network 被错误覆盖: %+v", got)
	}
}

func TestLebaraUKRFLockRejectsNetworkAndCellular(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openTestDB(t)

	iccid := "8944000000000000087"
	_ = db.UpsertCardPolicy(db.CardPolicy{ICCID: iccid, AirplaneEnabled: true, NetworkEnabled: false, IPVersion: "v4", Source: "auto"})
	p := device.NewPool(&config.Config{})
	w := &device.Worker{ID: "wwan-lebara", Config: config.DeviceConfig{ID: "wwan-lebara"}}
	setNestedPrivateField(t, w, []string{"state", "Identity", "ICCID"}, iccid)
	setNestedPrivateField(t, w, []string{"state", "Identity", "IMSI"}, "234870000000001")
	injectWorker(p, w)

	s := &Server{pool: p}
	r := gin.New()
	r.PATCH("/api/devices/:device_id/network", s.handleDeviceNetworkPatch)
	r.PATCH("/api/devices/:device_id/vowifi", s.handleDeviceVoWiFiPatch)
	r.PUT("/api/cards/:iccid/policy", s.handlePutCardPolicy)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/devices/wwan-lebara/network", strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("network enable code=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPatch, "/api/devices/wwan-lebara/vowifi", strings.NewReader(`{"enabled":true,"mode":"cellular"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("cellular mode code=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/cards/"+iccid+"/policy", strings.NewReader(`{"network_enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("put policy code=%d body=%s", rec.Code, rec.Body.String())
	}
	got, err := db.GetCardPolicy(iccid)
	if err != nil {
		t.Fatal(err)
	}
	if got.NetworkEnabled {
		t.Fatalf("Lebara 拒绝后不得改写 card-policy: %+v", got)
	}
}

func TestLebaraUKRFLockClassificationErrorBlocksNetworkMutation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openTestDB(t)
	iccid := "8944000000000000087"
	p := device.NewPool(&config.Config{})
	w := &device.Worker{ID: "wwan-lebara-error", Config: config.DeviceConfig{ID: "wwan-lebara-error"}}
	setNestedPrivateField(t, w, []string{"state", "Identity", "ICCID"}, iccid)
	setNestedPrivateField(t, w, []string{"state", "Identity", "IMSI"}, "204040000000001")
	injectWorker(p, w)
	sqlDB, err := db.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.DB = nil })

	s := &Server{pool: p}
	r := gin.New()
	r.PATCH("/api/devices/:device_id/network", s.handleDeviceNetworkPatch)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/devices/wwan-lebara-error/network", strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("network enable code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "识别 Lebara UK 射频策略失败") {
		t.Fatalf("classification failure was hidden: %s", rec.Body.String())
	}
}

func TestPutCardPolicyMWIMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		body      string
		code      int
		mode      string
		redetects bool
	}{
		{body: `{"mwi_mode":"on"}`, code: http.StatusOK, mode: "on", redetects: true},
		{body: `{"mwi_mode":"off"}`, code: http.StatusOK, mode: "off"},
		{body: `{"mwi_mode":"auto"}`, code: http.StatusOK, mode: "auto"},
		{body: `{"apn":"ims"}`, code: http.StatusOK, mode: "on"},
		{body: `{"mwi_mode":"maybe"}`, code: http.StatusBadRequest, mode: "on"},
	} {
		store := &cardPolicyStoreStub{policy: db.CardPolicy{ICCID: "8986005", MWIMode: "on", Source: "user"}}
		s := &Server{cardPolicies: store}
		r := gin.New()
		r.PUT("/api/cards/:iccid/policy", s.handlePutCardPolicy)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/cards/8986005/policy", strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != tc.code || store.policy.MWIMode != tc.mode {
			t.Fatalf("%s: code=%d mode=%q body=%s", tc.body, w.Code, store.policy.MWIMode, w.Body.String())
		}
		if redetected := len(store.cleared) == 1 && store.cleared[0] == "8986005"; redetected != tc.redetects {
			t.Fatalf("%s: cleared=%v", tc.body, store.cleared)
		}
	}
}

func TestPutCardPolicyMWIRedetectReconnectsOnlyWhenARejectionWasCleared(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		remembered, vowifi, reconnect bool
	}{{true, true, true}, {false, true, false}, {true, false, false}} {
		store := &cardPolicyStoreStub{
			policy:     db.CardPolicy{ICCID: "8986005", MWIMode: "auto", VoWiFiEnabled: tc.vowifi, Source: "user"},
			remembered: tc.remembered,
		}
		restarter := &cardPolicyRestarterStub{}
		s := &Server{cardPolicies: store, cardPolicyRestarter: restarter}
		r := gin.New()
		r.PUT("/api/cards/:iccid/policy", s.handlePutCardPolicy)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/cards/8986005/policy", strings.NewReader(`{"mwi_mode":"on"}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK || (restarter.iccid != "") != tc.reconnect {
			t.Fatalf("%+v: code=%d restarted=%q body=%s", tc, w.Code, restarter.iccid, w.Body.String())
		}
	}
}

func TestPutCardPolicyMWIClearFailureReportsSavedPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &cardPolicyStoreStub{
		policy:   db.CardPolicy{ICCID: "8986005", MWIMode: "off", Source: "user"},
		clearErr: errors.New("database is locked"),
	}
	s := &Server{cardPolicies: store}
	r := gin.New()
	r.PUT("/api/cards/:iccid/policy", s.handlePutCardPolicy)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/cards/8986005/policy", strings.NewReader(`{"mwi_mode":"on"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError || store.policy.MWIMode != "on" ||
		!strings.Contains(w.Body.String(), cardPolicySavedMWIClearFailedCode) {
		t.Fatalf("code=%d mode=%q body=%s", w.Code, store.policy.MWIMode, w.Body.String())
	}
}

func TestPutCardPolicyExplicitMWIRedetectReconnectsWithoutARecord(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &cardPolicyStoreStub{policy: db.CardPolicy{ICCID: "8986005", MWIMode: "on", VoWiFiEnabled: true, Source: "user"}}
	restarter := &cardPolicyRestarterStub{}
	s := &Server{cardPolicies: store, cardPolicyRestarter: restarter}
	r := gin.New()
	r.PUT("/api/cards/:iccid/policy", s.handlePutCardPolicy)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/cards/8986005/policy", strings.NewReader(`{"mwi_mode":"on","redetect_mwi":true}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK || restarter.iccid != "8986005" {
		t.Fatalf("code=%d restarted=%q body=%s", w.Code, restarter.iccid, w.Body.String())
	}
}
