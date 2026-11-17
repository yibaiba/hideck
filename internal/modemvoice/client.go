package modemvoice

import (
	"context"
	"errors"
	"strings"
	"time"
)

const (
	clccRetryDelay     = 250 * time.Millisecond
	queryTimeout       = 3 * time.Second
	callCommandTimeout = 60 * time.Second
)

// Executor is implemented by modem.Manager and shares its SMS/AT queue.
type Executor interface {
	ExecuteATContext(context.Context, string, time.Duration) (string, error)
}

type Client struct {
	executor Executor
	gate     chan struct{}
}

func NewClient(executor Executor) (*Client, error) {
	if executor == nil {
		return nil, errors.New("modem voice: AT executor is required")
	}
	return &Client{executor: executor, gate: make(chan struct{}, 1)}, nil
}

func (c *Client) lock(ctx context.Context) error {
	if ctx == nil {
		return errors.New("modem voice: context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case c.gate <- struct{}{}:
		return nil
	}
}

// Calls returns only successful, fully parsed snapshots. Poll failures retain
// the previous call state in the session rather than manufacturing an empty list.
func (c *Client) Calls(ctx context.Context) ([]Call, error) {
	if err := c.lock(ctx); err != nil {
		return nil, err
	}
	defer func() { <-c.gate }()
	return c.calls(ctx)
}

func (c *Client) calls(ctx context.Context) ([]Call, error) {
	calls, err := c.queryCalls(ctx)
	if !errors.Is(err, errCLCCRejectedSnapshot) {
		return calls, err
	}
	timer := time.NewTimer(clccRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, errors.Join(err, ctx.Err())
	case <-timer.C:
	}
	calls, retryErr := c.queryCalls(ctx)
	if retryErr != nil {
		return nil, errors.Join(err, retryErr)
	}
	return calls, nil
}

func dialCommand(number string) (string, error) {
	digits := number
	if strings.HasPrefix(digits, "+") {
		digits = digits[1:]
	}
	if digits == "" {
		return "", errors.New("modem voice: number is required")
	}
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return "", errors.New("modem voice: invalid dial number")
		}
	}
	return "ATD" + number + ";", nil
}

// Dial never retries: a timed-out ATD can still create a network call.
// A nil result means command acceptance, not remote answer or working audio.
func (c *Client) Dial(ctx context.Context, number string) error {
	command, err := dialCommand(number)
	if err != nil {
		return err
	}
	if err := c.lock(ctx); err != nil {
		return err
	}
	defer func() { <-c.gate }()
	calls, err := c.calls(ctx)
	if err != nil {
		return err
	}
	if len(voiceControlCalls(calls)) != 0 {
		return errors.New("modem voice: device already has a call")
	}
	_, err = c.executor.ExecuteATContext(ctx, command, callCommandTimeout)
	return err
}

// Control revalidates the expected call against a fresh CLCC snapshot.
func (c *Client) Answer(ctx context.Context, expected Call) error {
	return c.control(ctx, expected, true)
}

func (c *Client) Hangup(ctx context.Context, expected Call) error {
	return c.control(ctx, expected, false)
}

func (c *Client) control(ctx context.Context, expected Call, answer bool) error {
	if err := c.lock(ctx); err != nil {
		return err
	}
	defer func() { <-c.gate }()
	calls, err := c.calls(ctx)
	if err != nil {
		return err
	}
	command, err := controlCommand(calls, expected, answer)
	if err != nil {
		return err
	}
	_, err = c.executor.ExecuteATContext(ctx, command, callCommandTimeout)
	return err
}

// Established data/fax entries may coexist with voice. Pending data calls and
// unknown-mode entries stay visible: ATA could otherwise answer a data call.
func voiceControlCalls(calls []Call) []Call {
	var voice []Call
	for _, call := range calls {
		if !establishedDataCall(call) {
			voice = append(voice, call)
		}
	}
	return voice
}

func establishedDataCall(call Call) bool {
	return (call.Mode == 1 || call.Mode == 2) && call.State == Active
}

func sameCall(previous, current Call) bool {
	if (previous.State == Active || previous.State == Held) && current.State >= Dialing {
		return false
	}
	return previous.Index == current.Index && previous.Inbound == current.Inbound && previous.Mode == current.Mode &&
		(previous.Number == "" || current.Number == "" || previous.Number == current.Number)
}
