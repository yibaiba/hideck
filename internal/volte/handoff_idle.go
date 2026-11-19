package volte

import (
	"context"
	"fmt"
)

// IdleHandoff is bound at initialization to an AT-only Worker and SIM. It is
// not a fallback for a lost QMI connection and must never originate/end calls.
type IdleHandoff interface {
	Validate(context.Context) error
	CheckIdle(context.Context) error
}

func (c *Controller) confirmHandoffIdle(ctx context.Context, id string, s *session) error {
	c.mu.Lock()
	binding, started := s.idleHandoff, s.qmiVoiceStarted
	c.mu.Unlock()
	if binding == nil || started {
		return c.endCallsForHandoff(ctx, id)
	}
	if active := c.ActiveCall(id); active != nil {
		return fmt.Errorf("volte: local call remains; end the call before switching")
	}
	if err := binding.CheckIdle(ctx); err != nil {
		return fmt.Errorf("volte: AT idle check for handoff: %w", err)
	}
	return binding.Validate(ctx)
}
