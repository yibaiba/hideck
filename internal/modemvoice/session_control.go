package modemvoice

import (
	"context"
	"errors"
	"time"
)

func (s *Session) Dial(ctx context.Context, number string) (Update, error) {
	command, err := dialCommand(number)
	if err != nil {
		return Update{}, err
	}
	opCtx, release, err := s.enter(ctx)
	if err != nil {
		return Update{}, err
	}
	defer release()
	update, err := s.refresh(opCtx)
	if err != nil {
		return update, err
	}
	if len(s.voiceControlCalls()) != 0 {
		return update, errors.New("modem voice: device already has a call")
	}
	return s.execute(opCtx, command, update)
}

func (s *Session) Answer(ctx context.Context, id string) (Update, error) {
	return s.control(ctx, id, true)
}

func (s *Session) Hangup(ctx context.Context, id string) (Update, error) {
	return s.control(ctx, id, false)
}

func (s *Session) control(ctx context.Context, id string, answer bool) (Update, error) {
	opCtx, release, err := s.enter(ctx)
	if err != nil {
		return Update{}, err
	}
	defer release()
	if !containsCallID(s.tracker.Calls(), id) {
		return Update{}, ErrCallChanged
	}
	update, err := s.refresh(opCtx)
	if err != nil {
		return update, err
	}
	calls := s.voiceControlCalls()
	if !containsCallID(calls, id) {
		return update, ErrCallChanged
	}
	var expected Call
	var snapshot []Call
	for _, call := range calls {
		snapshot = append(snapshot, call.Call)
		if call.ID == id {
			expected = call.Call
		}
	}
	command, err := controlCommand(snapshot, expected, answer)
	if err != nil {
		return update, err
	}
	return s.execute(opCtx, command, update)
}

func (s *Session) voiceControlCalls() []TrackedCall {
	var voice []TrackedCall
	for _, call := range s.tracker.Calls() {
		if !establishedDataCall(call.Call) {
			voice = append(voice, call)
		}
	}
	return voice
}

func containsCallID(calls []TrackedCall, id string) bool {
	for _, call := range calls {
		if id != "" && call.ID == id {
			return true
		}
	}
	return false
}

func (s *Session) execute(ctx context.Context, command string, update Update) (Update, error) {
	return s.executeCommand(ctx, sessionCommand{text: command, timeout: callCommandTimeout}, update)
}

type sessionCommand struct {
	text    string
	timeout time.Duration
}

func (s *Session) executeCommand(ctx context.Context, command sessionCommand, update Update) (Update, error) {
	update.Attempted = true
	_, err := s.port.ExecuteATContext(ctx, command.text, command.timeout)
	update.Accepted = err == nil
	// Even a timed-out dial may have created a call; wake a fresh poll, never
	// redial automatically or manufacture an ended/connected state here.
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return update, err
}
