package host

import (
	"context"
	"errors"
	"time"
)

const runtimeCleanupTimeout = 60 * time.Second

func (c *Controller) Disable(ctx context.Context, id string) error {
	d := c.get(id)
	if d == nil {
		return nil
	}
	d.op.Lock()
	alreadyStopped := false
	select {
	case <-d.done:
		alreadyStopped = true
	default:
	}
	if d.call != nil && d.ctx.Err() == nil {
		if err := c.hangup(ctx, d, d.call); err != nil {
			d.op.Unlock()
			return err
		}
		if d.call != nil {
			d.op.Unlock()
			return errors.New("挂断期间模组出现新的来电，请处理后重试停用")
		}
	}
	for _, other := range d.otherCalls {
		if err := c.hangup(ctx, d, other); err != nil {
			d.op.Unlock()
			return err
		}
	}
	d.cancel()
	d.op.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-d.done:
	}
	return c.finishDisable(ctx, d, alreadyStopped)
}

func (c *Controller) finishDisable(ctx context.Context, d *device, retry bool) error {
	d.op.Lock()
	defer d.op.Unlock()
	// A later explicit stop retries unfinished cleanup. Serialize it with
	// resource transfer so old cleanup cannot close a replacement's runtime.
	if retry && d.cleanupErr != nil && !d.cleanupTransferred {
		d.cleanupErr = c.cleanupContext(ctx, d)
		phase := "stopped"
		if d.cleanupErr != nil {
			phase = "failed"
		}
		d.setStatus(phase, d.cleanupErr)
	}
	if d.cleanupErr != nil {
		return d.cleanupErr
	}
	c.mu.Lock()
	if c.devices[d.id] == d {
		delete(c.devices, d.id)
	}
	c.mu.Unlock()
	return nil
}

// Called only after previous.done; op also excludes an explicit cleanup retry.
func (c *Controller) takePendingCleanup(d *device) {
	previous := d.previous
	previous.op.Lock()
	defer previous.op.Unlock()
	if previous.cleanupErr == nil || previous.cleanupTransferred {
		return
	}
	d.resources, d.session = previous.resources, previous.session
	previous.resources, previous.session = nil, nil
	previous.cleanupTransferred = true
}

func (c *Controller) cleanup(d *device) error {
	return c.cleanupContext(context.Background(), d)
}

func (c *Controller) cleanupContext(parent context.Context, d *device) error {
	ctx, cancel := context.WithTimeout(parent, runtimeCleanupTimeout)
	defer cancel()
	err := c.endMedia(d, "mode_stopped")
	if d.session != nil {
		err = errors.Join(err, d.session.Close(ctx))
	}
	if d.resources != nil && d.resources.Close != nil {
		err = errors.Join(err, d.resources.Close(ctx))
	}
	return err
}
