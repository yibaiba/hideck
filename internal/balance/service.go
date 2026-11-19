package balance

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yibaiba/hideck/internal/carrierquery"
	"github.com/yibaiba/hideck/internal/modem"
)

type Service struct {
	gateway Gateway
	repo    Repository
	rules   RuleResolver
	now     func() time.Time
	timeout time.Duration
}

func NewService(gateway Gateway, repo Repository, rules RuleResolver) *Service {
	return &Service{gateway: gateway, repo: repo, rules: rules, now: time.Now, timeout: DefaultQueryTimeout}
}

func (s *Service) StartQuery(ctx context.Context, deviceID string) (Query, error) {
	snapshot, err := s.gateway.Snapshot(strings.TrimSpace(deviceID))
	if err != nil {
		return Query{}, err
	}
	rule, err := s.rules.Resolve(ctx, snapshot)
	if err != nil {
		return Query{}, err
	}
	if rule.Transport == carrierquery.TransportUnsupported {
		return Query{}, fmt.Errorf("%w: %s", ErrUnsupported, rule.Alternative)
	}
	query := s.newQuery(snapshot, rule)
	if err := s.repo.CreatePending(ctx, query); err != nil {
		return Query{}, err
	}
	sendErr := s.send(ctx, snapshot, rule, query.ID)
	// Submission may finish after the caller's deadline. Read the persisted
	// result without losing a late acknowledgement or an early reply.
	readbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	stored, _, err := s.repo.Get(readbackCtx, query.ID)
	if sendErr == nil || (err == nil && stored.State == StateCompleted) {
		return stored, err
	}
	_ = s.repo.MarkFailed(readbackCtx, query.ID, sendErr, s.now())
	failed, _, _ := s.repo.Get(readbackCtx, query.ID)
	return failed, sendErr
}

func (s *Service) send(ctx context.Context, snapshot DeviceSnapshot, rule carrierquery.Rule, queryID string) error {
	switch rule.Transport {
	case carrierquery.TransportSMS:
		if err := s.sendSMS(ctx, snapshot, rule); err != nil && !errors.Is(err, modem.ErrSMSSubmitUnknown) {
			return err
		}
		return s.repo.MarkAwaitingReply(context.Background(), queryID, s.now())
	case carrierquery.TransportUSSD:
		return s.sendUSSD(ctx, snapshot, rule, queryID)
	default:
		return fmt.Errorf("不支持的查询传输方式 %q", rule.Transport)
	}
}

func (s *Service) sendSMS(ctx context.Context, snapshot DeviceSnapshot, rule carrierquery.Rule) error {
	if snapshot.VoWiFiActive || snapshot.RouteSMSViaVoWiFi {
		return s.gateway.SendVoWiFiSMS(ctx, snapshot, rule.Destination, rule.Payload)
	}
	return s.gateway.SendBackendSMS(ctx, snapshot, rule.Destination, rule.Payload)
}

func (s *Service) sendUSSD(ctx context.Context, snapshot DeviceSnapshot, rule carrierquery.Rule, queryID string) error {
	var response USSDResponse
	var err error
	if snapshot.VoWiFiActive {
		response, err = s.gateway.SendVoWiFiUSSD(ctx, snapshot.DeviceID, rule.Payload)
	} else {
		response, err = s.gateway.SendBackendUSSD(ctx, snapshot.DeviceID, rule.Payload)
	}
	if err != nil {
		return err
	}
	if rule.ResponseMode == carrierquery.ResponseSMS {
		return s.repo.MarkAwaitingReply(context.Background(), queryID, s.now())
	}
	completion := parseResponse(rule, firstNonEmpty(response.Raw, response.Text))
	updated, err := s.repo.Complete(context.Background(), queryID, completion, s.now())
	if err != nil {
		return err
	}
	if !updated {
		return errors.New("余额查询已在响应返回前结束")
	}
	return nil
}

func (s *Service) HandleInboundSMS(ctx context.Context, message InboundSMS) (bool, error) {
	if message.Time.IsZero() {
		message.Time = s.now()
	}
	query, found, err := s.repo.FindPending(ctx, strings.TrimSpace(message.ICCID), message.Time)
	if err != nil || !found {
		return false, err
	}
	rule, err := s.rules.ByID(ctx, query.RuleID)
	if err != nil {
		return false, err
	}
	if !senderMatches(message.Sender, rule.ExpectedSenders) {
		return false, nil
	}
	return s.repo.Complete(ctx, query.ID, parseResponse(rule, message.Content), message.Time)
}

func (s *Service) Get(ctx context.Context, id string) (Query, bool, error) {
	return s.repo.Get(ctx, strings.TrimSpace(id))
}

func (s *Service) List(ctx context.Context, deviceID string, limit int, before *time.Time) ([]Query, error) {
	return s.repo.List(ctx, strings.TrimSpace(deviceID), limit, before)
}

func (s *Service) RunExpiry(ctx context.Context) error {
	if _, err := s.repo.ExpirePending(ctx, s.now()); err != nil {
		return err
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if _, err := s.repo.ExpirePending(ctx, s.now()); err != nil {
				return err
			}
		}
	}
}

func (s *Service) newQuery(snapshot DeviceSnapshot, rule carrierquery.Rule) Query {
	now := s.now()
	timeout := s.timeout
	if timeout <= 0 {
		timeout = DefaultQueryTimeout
	}
	return Query{ID: uuid.NewString(), DeviceID: snapshot.DeviceID, ICCID: snapshot.ICCID, RuleID: rule.ID,
		Transport: string(rule.Transport), State: StateSending, ParseState: ParsePending,
		Currency: rule.Currency, StartedAt: now, ExpiresAt: now.Add(timeout), CreatedAt: now, UpdatedAt: now}
}

func parseResponse(rule carrierquery.Rule, raw string) Completion {
	raw = strings.TrimSpace(raw)
	result := Completion{ParseState: ParseUnparsed, Currency: rule.Currency, RawResponse: raw,
		Summary: "已收到运营商回复，未能提取结构化余额"}
	if strings.TrimSpace(rule.ParserPattern) == "" {
		return result
	}
	expression, err := regexp.Compile(rule.ParserPattern)
	if err != nil {
		return result
	}
	match := expression.FindStringSubmatch(raw)
	amountIndex := expression.SubexpIndex("amount")
	if amountIndex <= 0 || amountIndex >= len(match) {
		return result
	}
	result.ParseState = ParseParsed
	result.Amount = strings.ReplaceAll(match[amountIndex], ",", ".")
	result.Summary = strings.TrimSpace(result.Amount + " " + rule.Currency)
	return result
}

func senderMatches(sender string, expected []string) bool {
	actual := normalizeSender(sender)
	for _, candidate := range expected {
		if actual != "" && actual == normalizeSender(candidate) {
			return true
		}
	}
	return false
}

func normalizeSender(value string) string {
	var builder strings.Builder
	for _, char := range strings.ToLower(strings.TrimSpace(value)) {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			builder.WriteRune(char)
		}
	}
	return builder.String()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
