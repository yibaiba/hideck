package modemvoice

import (
	"context"
	"errors"
	"testing"
	"time"
)

type partialResponseError struct{}

func (partialResponseError) Error() string             { return "device rejected CLCC" }
func (partialResponseError) PartialATResponse() string { return "+CLCC: 1,0,0,0,0\n+CME ERROR: 100" }

type clccSequence struct {
	queries  int
	first    error
	response string
	cancel   context.CancelFunc
}

func (p *clccSequence) ExecuteATContext(_ context.Context, _ string, _ time.Duration) (string, error) {
	p.queries++
	if p.queries == 1 {
		if p.cancel != nil {
			p.cancel()
		}
		return p.response, p.first
	}
	return "+CLCC: 2,1,5,0,0", nil
}

func TestCLCCRetriesRejectedPartialSnapshot(t *testing.T) {
	for _, first := range []error{nil, partialResponseError{}} {
		p := &clccSequence{first: first, response: "+CLCC: 1,0,0,0,0\n+CME ERROR: 100"}
		client, _ := NewClient(p)
		calls, err := client.Calls(context.Background())
		if err != nil || p.queries != 2 || len(calls) != 1 || calls[0].Index != 2 {
			t.Fatalf("partial snapshot retained: %+v %v queries=%d", calls, err, p.queries)
		}
	}
}

func TestCLCCNeverRetriesTransportFailure(t *testing.T) {
	p := &clccSequence{first: context.DeadlineExceeded, response: "+CLCC: 1,0,0,0,0\n+CME ERROR: 100"}
	client, _ := NewClient(p)
	if _, err := client.Calls(context.Background()); !errors.Is(err, context.DeadlineExceeded) || p.queries != 1 {
		t.Fatalf("err=%v queries=%d", err, p.queries)
	}
}

func TestCLCCRetryHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &clccSequence{first: partialResponseError{}, cancel: cancel}
	client, _ := NewClient(p)
	if _, err := client.Calls(ctx); !errors.Is(err, context.Canceled) || p.queries != 1 {
		t.Fatalf("err=%v queries=%d", err, p.queries)
	}
}
