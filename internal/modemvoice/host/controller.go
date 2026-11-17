// Package host connects AT call sessions to the application's phone gateway.
package host

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
	"github.com/yibaiba/hideck/internal/modemvoice"
	"github.com/yibaiba/hideck/internal/modemvoice/media"
)

type Resources struct {
	Port    modemvoice.Port
	Check   func(context.Context) error // Existing session identity; also used while stopping.
	CanCall func(context.Context) error // Optional current policy check for dial/answer.
	Route   func() (media.AudioRoute, error)
	Close   func(context.Context) error
}

// ErrPrepareRestarting hands preparation to hotplug/recovery without reporting ready.
var ErrPrepareRestarting = errors.New("模组重启准备中")

type Options struct {
	Prepare  func(context.Context, string) (*Resources, error)
	Listen   func() (net.PacketConn, error)
	Identity func(string) string
}

type Controller struct {
	options       Options
	mu            sync.Mutex
	devices       map[string]*device
	incoming      []func(voicehost.IncomingCall)
	events        []func(voicehost.CallEvent)
	notifications map[string]*notificationQueue
}

type device struct {
	id                 string
	identity           string
	previous           *device
	ctx                context.Context
	cancel             context.CancelFunc
	done               chan struct{}
	op                 sync.Mutex // Serializes controls, polling and teardown for this generation.
	mu                 sync.Mutex // Snapshot readers never wait on serial/audio I/O.
	phase, lastError   string
	cleanupErr         error
	cleanupTransferred bool // Protected by op after done; replacement owns pending resources.
	call               *call
	otherCalls         map[string]*call
	resources          *Resources
	session            *modemvoice.Session
}

type call struct {
	snapshot      voicehost.CallSnapshot
	trackedID     string
	endReason     string // Set after a local hangup is accepted, until CLCC confirms it.
	dialUncertain bool   // ATD was sent, but its final response was lost.
	conn          net.PacketConn
	bridge        *media.Bridge
	cancelMedia   context.CancelFunc
}

func New(options Options) *Controller {
	return &Controller{options: options, devices: make(map[string]*device)}
}

// Enable prepares only an explicitly selected mode. Status reads have no effects.
func (c *Controller) Enable(parent context.Context, id string) {
	identity := id
	if c.options.Identity != nil {
		identity = c.options.Identity(id)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var previous *device
	if existing := c.devices[id]; existing != nil {
		select {
		case <-existing.done:
			previous = existing
		default:
			if existing.identity == identity && existing.ctx.Err() == nil {
				return
			}
			existing.cancel()
			previous = existing
		}
	}
	ctx, cancel := context.WithCancel(parent)
	d := &device{id: id, identity: identity, previous: previous, ctx: ctx, cancel: cancel, done: make(chan struct{}), phase: "preparing"}
	c.devices[id] = d
	go c.run(d)
}

func (c *Controller) get(id string) *device {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.devices[id]
}

func (d *device) setStatus(phase string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.phase, d.lastError = phase, ""
	if err != nil {
		d.lastError = err.Error()
	}
}

func (c *Controller) run(d *device) {
	defer close(d.done)
	defer d.cancel()
	var err error
	if d.previous != nil {
		// A replacement SIM must never prepare a second runtime before the old
		// generation has finished cleanup, including an in-flight preparation.
		<-d.previous.done
		c.takePendingCleanup(d)
		d.previous = nil
	}
	if d.resources != nil {
		d.cleanupErr = c.cleanup(d)
		if d.cleanupErr != nil {
			d.setStatus("failed", d.cleanupErr)
			return
		}
		d.resources, d.session = nil, nil
	}
	d.resources, err = c.options.Prepare(d.ctx, d.id)
	if err == nil {
		d.session, err = modemvoice.NewSession(d.ctx, d.resources.Port)
	}
	if err != nil {
		d.cleanupErr = c.cleanup(d)
		phase := "failed"
		if errors.Is(err, ErrPrepareRestarting) && d.cleanupErr == nil {
			phase = "restarting"
		}
		d.setStatus(phase, errors.Join(err, d.cleanupErr))
		return
	}
	d.setStatus("ready", nil)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if d.ctx.Err() != nil {
			break
		}
		d.op.Lock()
		err = d.resources.Check(d.ctx)
		if err != nil {
			d.op.Unlock()
			d.cancel()
			break
		} else {
			var update modemvoice.Update
			update, err = d.session.Refresh(d.ctx)
			err = errors.Join(err, c.apply(d, update))
			if err == nil {
				err = c.finishUnconfirmedDial(d)
			}
			if err == nil && c.ActiveCall(d.id) == nil {
				for _, tracked := range d.session.Calls() {
					if tracked.Call.Mode == 0 && tracked.Call.Inbound {
						c.incomingCall(d, tracked)
						break
					}
				}
			}
			if err == nil {
				err = callAudioError(d)
			}
		}
		d.op.Unlock()
		if err != nil && d.ctx.Err() == nil {
			d.setStatus("failed", err)
		}
		if err == nil {
			d.setStatus("ready", nil)
		}
		select {
		case <-d.ctx.Done():
		case <-ticker.C:
		}
	}
	d.op.Lock()
	d.cleanupErr = c.cleanup(d)
	d.op.Unlock()
	if errors.Is(err, context.Canceled) {
		err = nil
	}
	if failure := errors.Join(err, d.cleanupErr); failure != nil {
		d.setStatus("failed", failure)
	} else {
		d.setStatus("stopped", nil)
	}
}

func callAudioError(d *device) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.call != nil && d.call.bridge != nil {
		return d.call.bridge.Err()
	}
	return nil
}

func (c *Controller) DeviceStatus(id string) map[string]interface{} {
	status := map[string]interface{}{"backend": "modem_voice", "ready": false, "phase": "idle"}
	if d := c.get(id); d != nil {
		d.mu.Lock()
		defer d.mu.Unlock()
		status["ready"], status["phase"], status["last_error"] = d.phase == "ready", d.phase, d.lastError
	}
	return status
}

func (c *Controller) ActiveCall(id string) *voicehost.CallSnapshot {
	d := c.get(id)
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.call == nil {
		return nil
	}
	snapshot := d.call.snapshot
	return &snapshot
}
