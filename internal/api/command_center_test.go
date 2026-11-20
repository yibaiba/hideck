package api

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/yibaiba/hideck/internal/balance"
	"github.com/yibaiba/hideck/internal/carrierquery"
	"github.com/yibaiba/hideck/internal/commandcenter"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/db"
	"github.com/yibaiba/hideck/internal/notify"
	"gorm.io/gorm"
)

type apiBalanceGateway struct {
	smsCalls int
}

func (g *apiBalanceGateway) Snapshot(deviceID string) (balance.DeviceSnapshot, error) {
	return balance.DeviceSnapshot{DeviceID: deviceID, ICCID: "iccid-1", MCC: "234", MNC: "10"}, nil
}

func (g *apiBalanceGateway) SendVoWiFiSMS(context.Context, balance.DeviceSnapshot, string, string) error {
	return errors.New("unexpected VoWiFi route")
}

func (g *apiBalanceGateway) SendBackendSMS(context.Context, balance.DeviceSnapshot, string, string) error {
	g.smsCalls++
	return nil
}

func (*apiBalanceGateway) SendVoWiFiUSSD(context.Context, string, string) (balance.USSDResponse, error) {
	return balance.USSDResponse{}, errors.New("unexpected VoWiFi USSD route")
}

func (*apiBalanceGateway) SendBackendUSSD(context.Context, string, string) (balance.USSDResponse, error) {
	return balance.USSDResponse{}, errors.New("unexpected backend USSD route")
}

type apiRuleResolver struct{}

func (apiRuleResolver) Resolve(context.Context, balance.DeviceSnapshot) (carrierquery.Rule, error) {
	return carrierquery.Rule{ID: "giffgaff", MCC: "234", MNC: "10", Operator: "giffgaff",
		Transport: carrierquery.TransportSMS, Destination: "85075", Payload: "INFO",
		ResponseMode: carrierquery.ResponseSMS, ExpectedSenders: []string{"giffgaff"}, Enabled: true}, nil
}

func (apiRuleResolver) ByID(context.Context, string) (carrierquery.Rule, error) {
	return apiRuleResolver{}.Resolve(context.Background(), balance.DeviceSnapshot{})
}

type apiCarrierRuleStore struct {
	rules     map[string]carrierquery.Rule
	saveCalls int
}

func (s *apiCarrierRuleStore) ListCustomCarrierQueryRules() ([]carrierquery.Rule, error) {
	rules := make([]carrierquery.Rule, 0, len(s.rules))
	for _, rule := range s.rules {
		rules = append(rules, rule)
	}
	return rules, nil
}

func (s *apiCarrierRuleStore) SaveCustomCarrierQueryRule(rule carrierquery.Rule) error {
	if err := rule.Validate(); err != nil {
		return err
	}
	s.saveCalls++
	s.rules[rule.ID] = rule
	return nil
}

func (s *apiCarrierRuleStore) DeleteCustomCarrierQueryRule(id string) error {
	delete(s.rules, id)
	return nil
}

func TestCommandCenterRoutesRequireAuthAndPersistExecution(t *testing.T) {
	server, token, _ := newCommandCenterAPITestServer(t)
	router := server.newRouter()
	unauthorized := performAPIRequest(router, apiRequestOptions{method: http.MethodGet, path: "/api/command-center/commands"})
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}
	body := bytes.NewBufferString(`{"input":"/list"}`)
	accepted := performAPIRequest(router, apiRequestOptions{method: http.MethodPost, path: "/api/command-center/executions", token: token, body: body})
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("execute status = %d body=%s", accepted.Code, accepted.Body.String())
	}
	waitForAPIEvents(t, server.commandCenter, 2)
	events := performAPIRequest(router, apiRequestOptions{method: http.MethodGet, path: "/api/command-center/events?after_id=0", token: token})
	if events.Code != http.StatusOK || !strings.Contains(events.Body.String(), `"state":"completed"`) ||
		!strings.Contains(events.Body.String(), `"source":"web"`) {
		t.Fatalf("events status=%d body=%s", events.Code, events.Body.String())
	}
}

func TestBalanceQueryRouteUsesInjectedProductionService(t *testing.T) {
	server, token, gateway := newCommandCenterAPITestServer(t)
	response := performAPIRequest(server.newRouter(), apiRequestOptions{
		method: http.MethodPost, path: "/api/devices/wwan0/balance-queries", token: token,
	})
	if response.Code != http.StatusAccepted || gateway.smsCalls != 1 {
		t.Fatalf("balance status=%d calls=%d body=%s", response.Code, gateway.smsCalls, response.Body.String())
	}
}

func TestApprovedCommandCenterRoutesAreProtected(t *testing.T) {
	server, _, _ := newCommandCenterAPITestServer(t)
	router := server.newRouter()
	routes := []apiRequestOptions{
		{method: http.MethodGet, path: "/api/command-center/commands"},
		{method: http.MethodPost, path: "/api/command-center/executions"},
		{method: http.MethodGet, path: "/api/command-center/events"},
		{method: http.MethodGet, path: "/api/command-center/stream"},
		{method: http.MethodGet, path: "/api/command-center/recordings/call_test.mp3"},
		{method: http.MethodDelete, path: "/api/command-center/history"},
		{method: http.MethodGet, path: "/api/balances"},
		{method: http.MethodPost, path: "/api/devices/wwan0/balance-queries"},
		{method: http.MethodGet, path: "/api/devices/wwan0/balance-queries"},
		{method: http.MethodGet, path: "/api/devices/wwan0/manual-balance"},
		{method: http.MethodPut, path: "/api/devices/wwan0/manual-balance"},
		{method: http.MethodDelete, path: "/api/devices/wwan0/manual-balance"},
		{method: http.MethodGet, path: "/api/carrier-query-rules"},
		{method: http.MethodPost, path: "/api/carrier-query-rules"},
		{method: http.MethodPut, path: "/api/carrier-query-rules/custom"},
		{method: http.MethodDelete, path: "/api/carrier-query-rules/custom"},
	}
	for _, route := range routes {
		response := performAPIRequest(router, route)
		if response.Code != http.StatusUnauthorized {
			t.Errorf("%s %s status = %d, want 401", route.method, route.path, response.Code)
		}
	}
}

func TestManualBalanceRoutesPersistUpdateAndDeleteWithoutSending(t *testing.T) {
	server, token, gateway := newCommandCenterAPITestServer(t)
	router := server.newRouter()
	put := func(body string) *httptest.ResponseRecorder {
		return performAPIRequest(router, apiRequestOptions{
			method: http.MethodPut, path: "/api/devices/wwan0/manual-balance", token: token,
			body: bytes.NewBufferString(body),
		})
	}
	created := put(`{"amount":"12.89","currency":"gbp"}`)
	if created.Code != http.StatusOK || !strings.Contains(created.Body.String(), `"transport":"manual"`) {
		t.Fatalf("create manual balance status=%d body=%s", created.Code, created.Body.String())
	}
	updated := put(`{"amount":"9.50","currency":"GBP"}`)
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), `"amount":"9.50"`) {
		t.Fatalf("update manual balance status=%d body=%s", updated.Code, updated.Body.String())
	}
	current := performAPIRequest(router, apiRequestOptions{
		method: http.MethodGet, path: "/api/devices/wwan0/manual-balance", token: token,
	})
	if current.Code != http.StatusOK || !strings.Contains(current.Body.String(), `"amount":"9.50"`) {
		t.Fatalf("get manual balance status=%d body=%s", current.Code, current.Body.String())
	}
	listed := performAPIRequest(router, apiRequestOptions{method: http.MethodGet, path: "/api/balances", token: token})
	if listed.Code != http.StatusOK || strings.Count(listed.Body.String(), `"transport":"manual"`) != 1 {
		t.Fatalf("list manual balance status=%d body=%s", listed.Code, listed.Body.String())
	}
	if gateway.smsCalls != 0 {
		t.Fatalf("manual balance sent SMS, calls=%d", gateway.smsCalls)
	}
	deleted := performAPIRequest(router, apiRequestOptions{
		method: http.MethodDelete, path: "/api/devices/wwan0/manual-balance", token: token,
	})
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete manual balance status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	listed = performAPIRequest(router, apiRequestOptions{method: http.MethodGet, path: "/api/balances", token: token})
	if strings.Contains(listed.Body.String(), `"transport":"manual"`) {
		t.Fatalf("manual balance remains after delete: %s", listed.Body.String())
	}
	current = performAPIRequest(router, apiRequestOptions{
		method: http.MethodGet, path: "/api/devices/wwan0/manual-balance", token: token,
	})
	if current.Code != http.StatusOK || !strings.Contains(current.Body.String(), `"query":null`) {
		t.Fatalf("get cleared manual balance status=%d body=%s", current.Code, current.Body.String())
	}
}

func TestManualBalanceRouteRejectsInvalidAmount(t *testing.T) {
	server, token, _ := newCommandCenterAPITestServer(t)
	response := performAPIRequest(server.newRouter(), apiRequestOptions{
		method: http.MethodPut, path: "/api/devices/wwan0/manual-balance", token: token,
		body: bytes.NewBufferString(`{"amount":"unknown","currency":"GBP"}`),
	})
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "金额必须是数字") {
		t.Fatalf("invalid manual balance status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestCommandRecordingServesOnlyInjectedMP3Files(t *testing.T) {
	server, token, _ := newCommandCenterAPITestServer(t)
	directory := t.TempDir()
	server.SetVoiceRecordingDirectory(directory)
	name := "call_wwan1_20260813_100108.890350649.mp3"
	want := []byte("ID3-test-audio")
	if err := os.WriteFile(filepath.Join(directory, name), want, 0o600); err != nil {
		t.Fatal(err)
	}
	router := server.newRouter()
	response := performAPIRequest(router, apiRequestOptions{
		method: http.MethodGet, path: "/api/command-center/recordings/" + name, token: token,
	})
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("recording status=%d type=%q body=%q", response.Code, response.Header().Get("Content-Type"), response.Body.Bytes())
	}
	if !bytes.Equal(response.Body.Bytes(), want) {
		t.Fatalf("recording body=%q want=%q", response.Body.Bytes(), want)
	}
	invalid := performAPIRequest(router, apiRequestOptions{
		method: http.MethodGet, path: "/api/command-center/recordings/not-a-call.mp3", token: token,
	})
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid filename status=%d body=%s", invalid.Code, invalid.Body.String())
	}
}

func TestCommandRecordingRejectsSymlinkEscape(t *testing.T) {
	server, token, _ := newCommandCenterAPITestServer(t)
	directory := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.mp3")
	if err := os.WriteFile(outside, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	name := "call_escape.mp3"
	if err := os.Symlink(outside, filepath.Join(directory, name)); err != nil {
		t.Fatal(err)
	}
	server.SetVoiceRecordingDirectory(directory)
	response := performAPIRequest(server.newRouter(), apiRequestOptions{
		method: http.MethodGet, path: "/api/command-center/recordings/" + name, token: token,
	})
	if response.Code != http.StatusNotFound {
		t.Fatalf("symlink status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestCommandRecordingSupportsByteRanges(t *testing.T) {
	server, token, _ := newCommandCenterAPITestServer(t)
	directory := t.TempDir()
	name := "call_range.mp3"
	if err := os.WriteFile(filepath.Join(directory, name), []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	server.SetVoiceRecordingDirectory(directory)
	request := httptest.NewRequest(http.MethodGet, "/api/command-center/recordings/"+name, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Range", "bytes=2-5")
	response := httptest.NewRecorder()
	server.newRouter().ServeHTTP(response, request)
	if response.Code != http.StatusPartialContent || response.Body.String() != "2345" {
		t.Fatalf("range status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestCarrierRuleCollectionPostValidatesReplySenders(t *testing.T) {
	server, token, _ := newCommandCenterAPITestServer(t)
	store := server.carrierRules.(*apiCarrierRuleStore)
	body := bytes.NewBufferString(`{"id":"custom","mcc":"234","mnc":"10","operator":"custom","transport":"sms","destination":"85075","payload":"INFO","response_mode":"sms","enabled":true}`)
	response := performAPIRequest(server.newRouter(), apiRequestOptions{
		method: http.MethodPost, path: "/api/carrier-query-rules", token: token, body: body,
	})
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "预期发送者") {
		t.Fatalf("empty sender status=%d body=%s", response.Code, response.Body.String())
	}
	if store.saveCalls != 0 {
		t.Fatalf("invalid rule persisted, save calls=%d", store.saveCalls)
	}

	validBody := bytes.NewBufferString(`{"id":"custom","mcc":"234","mnc":"10","operator":"custom","transport":"sms","destination":"85075","payload":"INFO","response_mode":"sms","expected_senders":["85075"],"enabled":true}`)
	valid := performAPIRequest(server.newRouter(), apiRequestOptions{
		method: http.MethodPost, path: "/api/carrier-query-rules", token: token, body: validBody,
	})
	if valid.Code != http.StatusOK || store.saveCalls != 1 {
		t.Fatalf("valid rule status=%d calls=%d body=%s", valid.Code, store.saveCalls, valid.Body.String())
	}
}

func TestCarrierRuleOverrideDeleteRestoresBuiltInCollectionEntry(t *testing.T) {
	server, token, _ := newCommandCenterAPITestServer(t)
	router := server.newRouter()
	builtIn := carrierquery.BuiltInRules()[0]
	override := builtIn
	override.BuiltIn = false
	override.Operator = "Database override"
	store := server.carrierRules.(*apiCarrierRuleStore)
	store.rules[override.ID] = override

	before := performAPIRequest(router, apiRequestOptions{method: http.MethodGet, path: "/api/carrier-query-rules", token: token})
	if before.Code != http.StatusOK || !strings.Contains(before.Body.String(), `"operator":"Database override"`) {
		t.Fatalf("list override status=%d body=%s", before.Code, before.Body.String())
	}
	deleted := performAPIRequest(router, apiRequestOptions{
		method: http.MethodDelete, path: "/api/carrier-query-rules/" + override.ID, token: token,
	})
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete override status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	after := performAPIRequest(router, apiRequestOptions{method: http.MethodGet, path: "/api/carrier-query-rules", token: token})
	if after.Code != http.StatusOK || strings.Contains(after.Body.String(), `"operator":"Database override"`) {
		t.Fatalf("list restored status=%d body=%s", after.Code, after.Body.String())
	}
	if !strings.Contains(after.Body.String(), `"id":"`+builtIn.ID+`"`) {
		t.Fatalf("built-in %q missing after restore: %s", builtIn.ID, after.Body.String())
	}
}

func TestCommandEventStreamResumesAfterLastEventID(t *testing.T) {
	server, token, _ := newCommandCenterAPITestServer(t)
	if _, err := server.commandCenter.Execute(context.Background(), commandcenter.ExecuteRequest{Input: "/list"}); err != nil {
		t.Fatal(err)
	}
	events := waitForAPIEvents(t, server.commandCenter, 2)
	httpServer := httptest.NewServer(server.newRouter())
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/command-center/stream", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Last-Event-ID", fmt.Sprint(events[0].ID))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	if !scanner.Scan() || scanner.Text() != ": connected" {
		t.Fatalf("initial stream frame = %q, want connected comment", scanner.Text())
	}
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "id:") {
			if got := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "id:")); got != fmt.Sprint(events[1].ID) {
				t.Fatalf("resumed event id = %s, want %d", got, events[1].ID)
			}
			return
		}
	}
	t.Fatalf("stream ended without event: %v", scanner.Err())
}

func TestCommandEventStreamWithoutCursorStartsAfterExistingHistory(t *testing.T) {
	server, token, _ := newCommandCenterAPITestServer(t)
	if _, err := server.commandCenter.Execute(context.Background(), commandcenter.ExecuteRequest{Input: "/list"}); err != nil {
		t.Fatal(err)
	}
	existing := waitForAPIEvents(t, server.commandCenter, 2)
	httpServer := httptest.NewServer(server.newRouter())
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/command-center/stream", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	if !scanner.Scan() || scanner.Text() != ": connected" {
		t.Fatalf("initial stream frame = %q, want connected comment", scanner.Text())
	}
	if _, err := server.commandCenter.Execute(context.Background(), commandcenter.ExecuteRequest{Input: "/list"}); err != nil {
		t.Fatal(err)
	}
	for scanner.Scan() {
		if !strings.HasPrefix(scanner.Text(), "id:") {
			continue
		}
		got, parseErr := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "id:")), 10, 64)
		if parseErr != nil || got <= existing[len(existing)-1].ID {
			t.Fatalf("first live event id = %d, want greater than existing %d", got, existing[len(existing)-1].ID)
		}
		return
	}
	t.Fatalf("stream ended without a new event: %v", scanner.Err())
}

type writeDeadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlineCalls int
	deadline      time.Time
}

func (w *writeDeadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	w.deadlineCalls++
	w.deadline = deadline
	return nil
}

func TestOnlyCommandEventStreamsDisableWriteDeadline(t *testing.T) {
	tests := []struct {
		path      string
		wantCalls int
	}{
		{path: "/api/command-center/stream", wantCalls: 1},
		{path: "/api/commands/events/stream", wantCalls: 1},
		{path: "/api/command-center/events", wantCalls: 0},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			writer := &writeDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			handler := withCommandEventStreamDeadlineDisabled(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			handler.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if writer.deadlineCalls != tt.wantCalls {
				t.Fatalf("SetWriteDeadline calls = %d, want %d", writer.deadlineCalls, tt.wantCalls)
			}
			if tt.wantCalls > 0 && !writer.deadline.IsZero() {
				t.Fatalf("stream write deadline = %s, want disabled", writer.deadline)
			}
		})
	}
}

func newCommandCenterAPITestServer(t *testing.T) (*Server, string, *apiBalanceGateway) {
	t.Helper()
	database, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&db.CommandExecution{}, &db.CommandEvent{}, &db.BalanceQuery{}); err != nil {
		t.Fatal(err)
	}
	commands := notify.NewCommandService(map[string]notify.CommandHandler{
		"list": func(_ notify.CommandContext, _ []string) string { return "设备列表 / 完成" },
	})
	gateway := &apiBalanceGateway{}
	server := &Server{auth: config.WebConfig{Username: "admin", Password: "secret"},
		commandCenter: commandcenter.NewService(commands, commandcenter.NewDatabaseStore(database)),
		balance:       balance.NewService(gateway, balance.NewDatabaseStore(database), apiRuleResolver{}),
		carrierRules:  &apiCarrierRuleStore{rules: make(map[string]carrierquery.Rule)},
		shutdownCh:    make(chan struct{}),
	}
	token, _, err := server.issueSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	return server, token, gateway
}

type apiRequestOptions struct {
	method string
	path   string
	token  string
	body   *bytes.Buffer
}

func performAPIRequest(handler http.Handler, options apiRequestOptions) *httptest.ResponseRecorder {
	var request *http.Request
	if options.body == nil {
		request = httptest.NewRequest(options.method, options.path, nil)
	} else {
		request = httptest.NewRequest(options.method, options.path, options.body)
		request.Header.Set("Content-Type", "application/json")
	}
	if options.token != "" {
		request.Header.Set("Authorization", "Bearer "+options.token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func waitForAPIEvents(t *testing.T, service *commandcenter.Service, count int) []commandcenter.Event {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		events, err := service.ListEvents(context.Background(), 0, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) >= count {
			return events
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for command events")
	return nil
}
