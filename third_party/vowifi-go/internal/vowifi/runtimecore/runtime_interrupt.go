package runtimecore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/engine/swu"
	"github.com/iniwex5/vowifi-go/internal/vowifi/logging"
	"go.uber.org/zap"
)

func runUntilInterrupted(
	ctx context.Context,
	req *RuntimeStartRequest,
	session *SessionResult,
) error {
	stopper := req.StopSession
	if stopper == nil {
		stopper = defaultStopSession
	}
	current := session
	for {
		bindSessionInterrupts(ctx, req, current)
		if req.Hooks.OnInterruptReady != nil {
			req.Hooks.OnInterruptReady(ctx)
		}
		outcome := waitRuntimeInterruption(ctx, req, current)
		if outcome.Kind == "reauth" {
			successor, err := startOverlappingReauth(ctx, req, current)
			if err != nil {
				logging.Info("overlapping IKE reauth failed; keeping old SA",
					"device", req.DeviceID, "trace_id", req.TraceID, "error", err)
				continue
			}
			old := current
			current = successor
			if req.voiceBinding != nil && successor != nil && successor.IMSService != nil {
				req.voiceBinding.AttachIfReady(successor.IMSService)
			}
			stopper(context.Background(), old)
			if old != nil && old.cleanupErr != nil {
				zap.S().Warnw("retired SWu session cleanup failed",
					"device", req.DeviceID, "trace_id", req.TraceID, "error", old.cleanupErr)
			}
			continue
		}
		stopper(context.Background(), current)
		if req.voiceBinding != nil {
			req.voiceBinding.Detach()
		}
		emitStopped(ctx, req, current)
		err := interruptionError(ctx, outcome, current)
		if current != nil && current.cleanupErr != nil {
			err = errors.Join(err, current.cleanupErr)
		}
		return err
	}
}

func interruptionError(
	ctx context.Context,
	outcome InterruptOutcome,
	session *SessionResult,
) error {
	switch outcome.Kind {
	case "context_cancelled":
		return ctx.Err()
	case "redirect":
		return &ErrRedirect{NewEPDG: outcome.RedirectEPDG, Delay: outcome.RetryDelay}
	case "reauth":
		return swu.ErrFreshRuntimeRequired
	case "session_down":
		if session != nil && session.Session != nil {
			if err := session.Session.TerminalError(); err != nil {
				return err
			}
		}
		return errors.New(firstNonEmpty(outcome.Reason, "runtimecore: SWu session stopped"))
	case "ims_reconnect":
		return errors.New(firstNonEmpty(outcome.Reason, "runtimecore: IMS reconnect requested"))
	default:
		return nil
	}
}

func waitRuntimeInterruption(
	ctx context.Context,
	req *RuntimeStartRequest,
	result *SessionResult,
) InterruptOutcome {
	var session *swu.Session
	if result != nil {
		session = result.Session
	}
	outcomes := bindSessionInterrupts(ctx, req, result)
	var imsErrors <-chan error
	if result != nil && result.IMSService != nil {
		imsErrors = result.IMSService.RegistrationErrors()
	}
	sessionDone := sessionDoneChan(session)
	select {
	case <-ctx.Done():
		outcome := InterruptOutcome{Kind: "context_cancelled", Reason: ctx.Err().Error()}
		emitInterrupted(ctx, req, result, outcome)
		return outcome
	case err, ok := <-imsErrors:
		reason := "IMS runtime reconnect requested"
		if ok && err != nil {
			reason = err.Error()
		}
		outcome := InterruptOutcome{Kind: "ims_reconnect", Reason: reason}
		emitInterrupted(ctx, req, result, outcome)
		return outcome
	case outcome := <-outcomes:
		if outcome.Kind != "reauth" {
			emitInterrupted(ctx, req, result, outcome)
		}
		return outcome
	case <-sessionDone:
		reason := "swu_session_down"
		if err := session.TerminalError(); err != nil {
			reason = err.Error()
		}
		outcome := InterruptOutcome{Kind: "session_down", Reason: reason}
		emitInterrupted(ctx, req, result, outcome)
		return outcome
	}
}

func bindSessionInterrupts(
	ctx context.Context,
	req *RuntimeStartRequest,
	result *SessionResult,
) <-chan InterruptOutcome {
	if result == nil || result.Session == nil {
		return nil
	}
	if result.interrupts != nil {
		return result.interrupts
	}
	result.interrupts = make(chan InterruptOutcome, 3)
	chainSessionCallbacks(sessionCallbackConfig{
		ctx: ctx, request: req, session: result.Session, outcomes: result.interrupts,
	})
	return result.interrupts
}

type sessionCallbackConfig struct {
	ctx      context.Context
	request  *RuntimeStartRequest
	session  *swu.Session
	outcomes chan<- InterruptOutcome
}

func chainSessionCallbacks(config sessionCallbackConfig) {
	chainSessionDown(config)
	chainSessionReauth(config)
	chainSessionRedirect(config)
}

func chainSessionDown(config sessionCallbackConfig) {
	previous := config.session.OnSessionDown
	config.session.OnSessionDown = func() {
		if previous != nil {
			previous()
		}
		if config.request.Hooks.OnSessionDown != nil {
			config.request.Hooks.OnSessionDown(config.ctx)
		}
		sendOutcome(config.outcomes, InterruptOutcome{Kind: "session_down", Reason: "swu_session_down"})
	}
}

func chainSessionReauth(config sessionCallbackConfig) {
	previous := config.session.OnReauthNeeded
	config.session.OnReauthNeeded = func() {
		if previous != nil {
			previous()
		}
		if config.request.Hooks.OnReauthNeeded != nil {
			config.request.Hooks.OnReauthNeeded(config.ctx)
		}
		sendOutcome(config.outcomes, InterruptOutcome{Kind: "reauth", Reason: swu.ErrFreshRuntimeRequired.Error()})
	}
}

func chainSessionRedirect(config sessionCallbackConfig) {
	previous := config.session.OnRedirect
	config.session.OnRedirect = func(target string) {
		if previous != nil {
			previous(target)
		}
		if config.request.Hooks.OnRedirect != nil {
			config.request.Hooks.OnRedirect(config.ctx, target)
		}
		sendOutcome(config.outcomes, InterruptOutcome{
			Kind: "redirect", Reason: "redirect", RedirectEPDG: strings.TrimSpace(target),
			RetryDelay: int64(2 * time.Second),
		})
	}
}

func sendOutcome(ch chan<- InterruptOutcome, outcome InterruptOutcome) {
	select {
	case ch <- outcome:
	default:
	}
}

func applyRedirectOverride(req *RuntimeStartRequest, err error) error {
	target := redirectTarget(err)
	if req == nil || target == "" {
		return err
	}
	if containsRedirectTarget(req.redirectSeen, target) || req.redirectHops >= maxSWuRedirects {
		return ErrTooManyRedirects
	}
	req.redirectHops++
	req.redirectSeen = append(req.redirectSeen, target)
	req.RuntimeEPDGOverride = target
	return err
}

func redirectTarget(err error) string {
	var redirect *ErrRedirect
	if errors.As(err, &redirect) {
		return firstNonEmpty(redirect.NewEPDG, redirect.Target)
	}
	return ""
}

func containsRedirectTarget(seen []string, target string) bool {
	for _, value := range seen {
		if value == target {
			return true
		}
	}
	return false
}
