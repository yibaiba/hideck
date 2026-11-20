package balance

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yibaiba/hideck/internal/carrierquery"
	appdb "github.com/yibaiba/hideck/internal/db"
	"github.com/yibaiba/hideck/internal/modem"
)

type fakeGateway struct {
	snapshot              DeviceSnapshot
	sendErr               error
	onBackendSMS          func()
	wifiSMS, backendSMS   int
	wifiUSSD, backendUSSD int
	ussdResponse          USSDResponse
}

func (g *fakeGateway) Snapshot(string) (DeviceSnapshot, error) { return g.snapshot, nil }
func (g *fakeGateway) SendVoWiFiSMS(context.Context, DeviceSnapshot, string, string) error {
	g.wifiSMS++
	return g.sendErr
}
func (g *fakeGateway) SendBackendSMS(context.Context, DeviceSnapshot, string, string) error {
	g.backendSMS++
	if g.onBackendSMS != nil {
		g.onBackendSMS()
	}
	return g.sendErr
}
func (g *fakeGateway) SendVoWiFiUSSD(context.Context, string, string) (USSDResponse, error) {
	g.wifiUSSD++
	return g.ussdResponse, g.sendErr
}
func (g *fakeGateway) SendBackendUSSD(context.Context, string, string) (USSDResponse, error) {
	g.backendUSSD++
	return g.ussdResponse, g.sendErr
}

type staticRules struct {
	rule carrierquery.Rule
}

func (r staticRules) Resolve(context.Context, DeviceSnapshot) (carrierquery.Rule, error) {
	return r.rule, nil
}
func (r staticRules) ByID(_ context.Context, id string) (carrierquery.Rule, error) {
	if id != r.rule.ID {
		return carrierquery.Rule{}, ErrRuleNotFound
	}
	return r.rule, nil
}

func newTestService(t *testing.T, gateway *fakeGateway, rule carrierquery.Rule, now time.Time) (*Service, *DatabaseStore) {
	t.Helper()
	if err := appdb.Init(filepath.Join(t.TempDir(), "balance.db")); err != nil {
		t.Fatalf("db.Init() error = %v", err)
	}
	t.Cleanup(func() { appdb.DB = nil })
	store := NewDatabaseStore(appdb.DB)
	service := NewService(gateway, store, staticRules{rule: rule})
	service.now = func() time.Time { return now }
	return service, store
}

func smsTestRule() carrierquery.Rule {
	return carrierquery.Rule{ID: "giffgaff", MCC: "234", MNC: "10", Operator: "giffgaff",
		Transport: carrierquery.TransportSMS, Destination: "85075", Payload: "INFO",
		ResponseMode: carrierquery.ResponseSMS, ExpectedSenders: []string{"giffgaff"},
		ParserPattern: `(?i)credit[^0-9]*(?P<amount>[0-9]+(?:\.[0-9]{2})?)`, Currency: "GBP", Enabled: true}
}

func TestStartSMSQueryUsesOnlyVoWiFiAndBlocksSecondPending(t *testing.T) {
	now := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	gateway := &fakeGateway{snapshot: DeviceSnapshot{DeviceID: "wwan0", ICCID: "iccid-1", MCC: "234", MNC: "10", VoWiFiActive: true}}
	service, _ := newTestService(t, gateway, smsTestRule(), now)

	query, err := service.StartQuery(context.Background(), "wwan0")
	if err != nil || query.State != StateAwaitingReply {
		t.Fatalf("StartQuery() = %+v, %v", query, err)
	}
	if gateway.wifiSMS != 1 || gateway.backendSMS != 0 {
		t.Fatalf("SMS calls wifi/backend = %d/%d", gateway.wifiSMS, gateway.backendSMS)
	}
	if _, err := service.StartQuery(context.Background(), "wwan0"); !errors.Is(err, ErrPendingQuery) {
		t.Fatalf("second StartQuery() error = %v", err)
	}
	if gateway.wifiSMS != 1 {
		t.Fatalf("pending query triggered another send, calls = %d", gateway.wifiSMS)
	}
}

func TestSendSMSKeepsVoWiFiRouteDuringRuntimeRecovery(t *testing.T) {
	gateway := &fakeGateway{}
	service := NewService(gateway, nil, nil)
	snapshot := DeviceSnapshot{DeviceID: "wwan0", RouteSMSViaVoWiFi: true}
	if err := service.sendSMS(context.Background(), snapshot, smsTestRule()); err != nil {
		t.Fatal(err)
	}
	if gateway.wifiSMS != 1 || gateway.backendSMS != 0 {
		t.Fatalf("SMS calls wifi/backend = %d/%d", gateway.wifiSMS, gateway.backendSMS)
	}
}

func TestStartQueryFailureIsPersistedWithoutRetry(t *testing.T) {
	now := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	gateway := &fakeGateway{snapshot: DeviceSnapshot{DeviceID: "wwan0", ICCID: "iccid-1", MCC: "234", MNC: "10"}, sendErr: errors.New("send failed")}
	service, _ := newTestService(t, gateway, smsTestRule(), now)

	query, err := service.StartQuery(context.Background(), "wwan0")
	if err == nil || query.State != StateFailed || query.Error != "send failed" {
		t.Fatalf("StartQuery() = %+v, %v", query, err)
	}
	if gateway.backendSMS != 1 || gateway.wifiSMS != 0 {
		t.Fatalf("SMS calls wifi/backend = %d/%d", gateway.wifiSMS, gateway.backendSMS)
	}
}

func TestSMSQueryWithUnknownSubmissionCanReceiveReply(t *testing.T) {
	now := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	gateway := &fakeGateway{
		snapshot: DeviceSnapshot{DeviceID: "wwan0", ICCID: "iccid-1", MCC: "234", MNC: "10"},
		sendErr:  modem.ErrSMSSubmitUnknown,
	}
	service, store := newTestService(t, gateway, smsTestRule(), now)
	request, cancel := context.WithCancel(context.Background())
	defer cancel()
	gateway.onBackendSMS = cancel
	query, err := service.StartQuery(request, "wwan0")
	if err != nil || query.State != StateAwaitingReply {
		t.Fatalf("StartQuery() = %+v, %v", query, err)
	}
	ctx := context.Background()
	if _, err := service.StartQuery(ctx, "wwan0"); !errors.Is(err, ErrPendingQuery) || gateway.backendSMS != 1 {
		t.Fatalf("pending query retried SMS: calls=%d err=%v", gateway.backendSMS, err)
	}
	matched, err := service.HandleInboundSMS(ctx, InboundSMS{
		ICCID: "iccid-1", Sender: "giffgaff", Content: "credit 5.00", Time: now.Add(time.Second),
	})
	if err != nil || !matched {
		t.Fatalf("HandleInboundSMS() = %t, %v", matched, err)
	}
	stored, _, err := store.Get(ctx, query.ID)
	if err != nil || stored.State != StateCompleted || stored.Amount != "5.00" {
		t.Fatalf("stored query = %+v, %v", stored, err)
	}
}

func TestUSSDQueryUsesSelectedRouteAndPersistsRawResponse(t *testing.T) {
	for _, test := range []struct {
		name   string
		vowifi bool
	}{
		{name: "vowifi", vowifi: true},
		{name: "backend", vowifi: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
			gateway := &fakeGateway{snapshot: DeviceSnapshot{DeviceID: "wwan0", ICCID: "iccid-1", MCC: "228", MNC: "02", VoWiFiActive: test.vowifi}, ussdResponse: USSDResponse{Raw: "Guthaben: 14.50"}}
			rule := carrierquery.Rule{ID: "sunrise", MCC: "228", MNC: "02", Operator: "Sunrise", Transport: carrierquery.TransportUSSD,
				Payload: "*121#", ResponseMode: carrierquery.ResponseDirect, ParserPattern: `(?i)guthaben[^0-9]*(?P<amount>[0-9]+\.[0-9]{2})`, Currency: "CHF", Enabled: true}
			service, _ := newTestService(t, gateway, rule, now)
			query, err := service.StartQuery(context.Background(), "wwan0")
			if err != nil || query.State != StateCompleted || query.Amount != "14.50" || query.RawResponse != "Guthaben: 14.50" {
				t.Fatalf("StartQuery() = %+v, %v", query, err)
			}
			if gateway.wifiUSSD+gateway.backendUSSD != 1 || (test.vowifi && gateway.backendUSSD != 0) || (!test.vowifi && gateway.wifiUSSD != 0) {
				t.Fatalf("USSD calls wifi/backend = %d/%d", gateway.wifiUSSD, gateway.backendUSSD)
			}
		})
	}
}

func TestInboundSMSRequiresExpectedSenderAndCompletesParsedQuery(t *testing.T) {
	now := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	gateway := &fakeGateway{snapshot: DeviceSnapshot{DeviceID: "wwan0", ICCID: "iccid-1", MCC: "234", MNC: "10"}}
	service, store := newTestService(t, gateway, smsTestRule(), now)
	query, err := service.StartQuery(context.Background(), "wwan0")
	if err != nil {
		t.Fatal(err)
	}

	matched, err := service.HandleInboundSMS(context.Background(), InboundSMS{ICCID: "iccid-1", Sender: "other", Content: "credit 99.00", Time: now.Add(time.Second)})
	if err != nil || matched {
		t.Fatalf("unexpected sender matched=%v err=%v", matched, err)
	}
	matched, err = service.HandleInboundSMS(context.Background(), InboundSMS{ICCID: "iccid-1", Sender: "GiffGaff", Content: "Your credit is 12.89 GBP", Time: now.Add(2 * time.Second)})
	if err != nil || !matched {
		t.Fatalf("expected sender matched=%v err=%v", matched, err)
	}
	stored, _, _ := store.Get(context.Background(), query.ID)
	if stored.State != StateCompleted || stored.ParseState != ParseParsed || stored.Amount != "12.89" || stored.RawResponse != "Your credit is 12.89 GBP" {
		t.Fatalf("stored query = %+v", stored)
	}
}

func TestInboundSMSMatchesPendingQueryAcrossTimezoneOffsets(t *testing.T) {
	queryZone := time.FixedZone("UTC+8", 8*60*60)
	replyZone := time.FixedZone("UTC+1", 1*60*60)
	now := time.Date(2026, 8, 12, 7, 28, 46, 183914565, queryZone)
	gateway := &fakeGateway{snapshot: DeviceSnapshot{
		DeviceID: "wwan1", ICCID: "iccid-cte", MCC: "234", MNC: "33",
	}}
	rule, found := carrierquery.FindBuiltIn("234", "33")
	if !found {
		t.Fatal("CTExcel built-in rule not found")
	}
	service, store := newTestService(t, gateway, rule, now)
	query, err := service.StartQuery(context.Background(), "wwan1")
	if err != nil {
		t.Fatal(err)
	}
	replyTime := now.Add(2 * time.Second).In(replyZone)

	matched, err := service.HandleInboundSMS(context.Background(), InboundSMS{
		ICCID: "iccid-cte", Sender: "888", Content: "您当前余额为 £1", Time: replyTime,
	})
	if err != nil || !matched {
		t.Fatalf("cross-timezone reply matched=%v err=%v query=%s reply=%s", matched, err, now, replyTime)
	}
	stored, _, err := store.Get(context.Background(), query.ID)
	if err != nil || stored.State != StateCompleted || stored.Amount != "1" || stored.Currency != "GBP" {
		t.Fatalf("stored query = %+v, err=%v", stored, err)
	}
}

func TestExpirePendingPreventsLateSMSCorrelation(t *testing.T) {
	now := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	gateway := &fakeGateway{snapshot: DeviceSnapshot{DeviceID: "wwan0", ICCID: "iccid-1", MCC: "234", MNC: "10"}}
	service, store := newTestService(t, gateway, smsTestRule(), now)
	query, err := service.StartQuery(context.Background(), "wwan0")
	if err != nil {
		t.Fatal(err)
	}
	late := now.Add(DefaultQueryTimeout + time.Second)
	if count, err := store.ExpirePending(context.Background(), late); err != nil || count != 1 {
		t.Fatalf("ExpirePending() = %d, %v", count, err)
	}
	matched, err := service.HandleInboundSMS(context.Background(), InboundSMS{ICCID: "iccid-1", Sender: "giffgaff", Content: "credit 1.00", Time: late})
	if err != nil || matched {
		t.Fatalf("late SMS matched=%v err=%v", matched, err)
	}
	stored, _, _ := store.Get(context.Background(), query.ID)
	if stored.State != StateTimedOut {
		t.Fatalf("stored state = %s", stored.State)
	}
}

func TestDatabaseStoreSerializesPendingCreationPerICCID(t *testing.T) {
	if err := appdb.Init(filepath.Join(t.TempDir(), "concurrent.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { appdb.DB = nil })
	store := NewDatabaseStore(appdb.DB)
	now := time.Now()
	queries := []Query{
		{ID: "one", DeviceID: "dev", ICCID: "same", RuleID: "rule", Transport: "sms", State: StateSending, ParseState: ParsePending, StartedAt: now, ExpiresAt: now.Add(time.Minute), CreatedAt: now, UpdatedAt: now},
		{ID: "two", DeviceID: "dev", ICCID: "same", RuleID: "rule", Transport: "sms", State: StateSending, ParseState: ParsePending, StartedAt: now, ExpiresAt: now.Add(time.Minute), CreatedAt: now, UpdatedAt: now},
	}
	errorsByCall := make([]error, len(queries))
	var wait sync.WaitGroup
	for index := range queries {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			errorsByCall[index] = store.CreatePending(context.Background(), queries[index])
		}(index)
	}
	wait.Wait()
	successes, conflicts := 0, 0
	for _, err := range errorsByCall {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrPendingQuery) {
			conflicts++
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent results = %v", errorsByCall)
	}
}

func TestManualBalanceUpsertsWithoutSendingAndCanBeCleared(t *testing.T) {
	now := time.Date(2026, 8, 14, 1, 0, 0, 0, time.UTC)
	gateway := &fakeGateway{snapshot: DeviceSnapshot{
		DeviceID: "wwan0", ICCID: "iccid-1", MCC: "234", MNC: "10",
	}}
	service, store := newTestService(t, gateway, smsTestRule(), now)

	first, err := service.SetManualBalance(context.Background(), "wwan0", "12,89", "gbp")
	if err != nil {
		t.Fatal(err)
	}
	if first.Transport != TransportManual || first.ParseState != ParseManual || first.Amount != "12.89" || first.Currency != "GBP" {
		t.Fatalf("first manual balance = %+v", first)
	}
	if gateway.wifiSMS+gateway.backendSMS+gateway.wifiUSSD+gateway.backendUSSD != 0 {
		t.Fatal("manual balance must not send SMS or USSD")
	}

	service.now = func() time.Time { return now.Add(time.Minute) }
	updated, err := service.SetManualBalance(context.Background(), "wwan0", "9.5", "GBP")
	if err != nil || updated.ID != first.ID || updated.Amount != "9.5" || !updated.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("updated manual balance = %+v, err=%v", updated, err)
	}
	queries, err := store.List(context.Background(), "wwan0", 50, nil)
	if err != nil || len(queries) != 1 || queries[0].Amount != "9.5" {
		t.Fatalf("manual query list = %+v, err=%v", queries, err)
	}
	current, found, err := service.GetManualBalance(context.Background(), "wwan0")
	if err != nil || !found || current.ID != first.ID || current.Amount != "9.5" {
		t.Fatalf("GetManualBalance() = %+v, %v, %v", current, found, err)
	}
	cleared, err := service.ClearManualBalance(context.Background(), "wwan0")
	if err != nil || !cleared {
		t.Fatalf("ClearManualBalance() = %v, %v", cleared, err)
	}
	if _, found, err := service.GetManualBalance(context.Background(), "wwan0"); err != nil || found {
		t.Fatalf("GetManualBalance() after clear found=%v err=%v", found, err)
	}
}

func TestManualBalanceRejectsInvalidAmountWithoutPersisting(t *testing.T) {
	now := time.Date(2026, 8, 14, 1, 0, 0, 0, time.UTC)
	gateway := &fakeGateway{snapshot: DeviceSnapshot{DeviceID: "wwan0", ICCID: "iccid-1"}}
	service, store := newTestService(t, gateway, smsTestRule(), now)

	if _, err := service.SetManualBalance(context.Background(), "wwan0", "twelve", "GBP"); !errors.Is(err, ErrInvalidManual) {
		t.Fatalf("SetManualBalance() error = %v", err)
	}
	queries, err := store.List(context.Background(), "wwan0", 50, nil)
	if err != nil || len(queries) != 0 {
		t.Fatalf("invalid manual balance persisted: %+v, err=%v", queries, err)
	}
}
