package vowifihost

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/yibaiba/hideck/pkg/logger"
)

type LifecycleCommandKind int

const (
	LifecycleCommandEnable LifecycleCommandKind = iota + 1
	LifecycleCommandDisable
	LifecycleCommandRestart
	LifecycleCommandRecover
	LifecycleCommandSwitchBegin
	LifecycleCommandSwitchEnd
)

func (k LifecycleCommandKind) String() string {
	switch k {
	case LifecycleCommandEnable:
		return "enable"
	case LifecycleCommandDisable:
		return "disable"
	case LifecycleCommandRestart:
		return "restart"
	case LifecycleCommandRecover:
		return "recover"
	case LifecycleCommandSwitchBegin:
		return "switch_begin"
	case LifecycleCommandSwitchEnd:
		return "switch_end"
	default:
		return fmt.Sprintf("unknown(%d)", int(k))
	}
}

type LifecycleCommand struct {
	DeviceID           string
	Kind               LifecycleCommandKind
	Reason             string
	OverrideEPDG       string
	RestoreRadio       bool
	AllowSwitch        bool
	RuntimeInvalidated bool
	Generation         uint64
	// QueueTimeout, when set, bounds only the wait for the previous command;
	// the command itself still runs with the caller's context.
	QueueTimeout time.Duration
}

type LifecycleControllerOptions struct {
	IsActive func(deviceID string) bool
	Run      func(context.Context, LifecycleCommand) error
}

type LifecycleController struct {
	mu                sync.Mutex
	devices           map[string]*deviceLifecycle
	isActive          func(deviceID string) bool
	run               func(context.Context, LifecycleCommand) error
	TestRun           func(context.Context, LifecycleCommand) error
	RecoverRunForTest func(context.Context, string, string, string) error
}

type deviceLifecycle struct {
	// runSlot serializes lifecycle commands (capacity 1). Waiters give up when
	// their context ends, so a runner that never exits does not hang callers.
	runSlot      chan struct{}
	generationMu sync.Mutex
	generation   uint64
	runCancel    context.CancelFunc
	runCancelSeq uint64
	invalidation uint64
}

func NewLifecycleController(options ...LifecycleControllerOptions) *LifecycleController {
	c := &LifecycleController{devices: make(map[string]*deviceLifecycle)}
	if len(options) > 0 {
		c.isActive = options[0].IsActive
		c.run = options[0].Run
	}
	return c
}

func (c *LifecycleController) SetRunForTest(fn func(context.Context, LifecycleCommand) error) {
	if c == nil {
		return
	}
	c.TestRun = fn
}

func (c *LifecycleController) SetRecoverRunForTest(fn func(context.Context, string, string, string) error) {
	if c == nil {
		return
	}
	c.RecoverRunForTest = fn
}

func (c *LifecycleController) Submit(ctx context.Context, cmd LifecycleCommand) error {
	if c == nil {
		return fmt.Errorf("vowifi lifecycle controller is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cmd.DeviceID = strings.TrimSpace(cmd.DeviceID)
	if cmd.DeviceID == "" {
		return fmt.Errorf("vowifi lifecycle command device_id is empty")
	}
	if cmd.Kind == 0 {
		return fmt.Errorf("vowifi lifecycle command kind is empty")
	}

	lifecycle := c.device(cmd.DeviceID)
	invalidation := c.invalidationVersion(lifecycle)
	if cmd.Kind == LifecycleCommandSwitchBegin || cmd.Kind == LifecycleCommandRestart {
		return c.submitPreempting(ctx, lifecycle, lifecycleSubmission{command: cmd, invalidation: invalidation, preempt: true})
	}

	if err := lifecycle.acquireRun(ctx, cmd); err != nil {
		return err
	}
	defer lifecycle.releaseRun()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if invalidation != c.invalidationVersion(lifecycle) {
		return context.Canceled
	}

	if c.commandGenerationStale(lifecycle, cmd) {
		currentGeneration := c.currentGeneration(lifecycle)
		logger.Debug("忽略过期 VoWiFi lifecycle 命令",
			"device", cmd.DeviceID,
			"kind", cmd.Kind.String(),
			"command_generation", cmd.Generation,
			"current_generation", currentGeneration,
			"reason", strings.TrimSpace(cmd.Reason))
		return nil
	}

	if cmd.Kind == LifecycleCommandEnable && c.isActive != nil && c.isActive(cmd.DeviceID) {
		logger.Debug("忽略重复 VoWiFi enable 命令",
			"device", cmd.DeviceID,
			"reason", strings.TrimSpace(cmd.Reason),
			"current_generation", c.currentGeneration(lifecycle))
		return nil
	}

	runCtx, cmd, clearRun := c.bindCommandRun(ctx, lifecycle, lifecycleSubmission{command: cmd, invalidation: invalidation})
	defer clearRun()
	if err := runCtx.Err(); err != nil {
		return err
	}
	return c.runCommand(runCtx, cmd)
}

func (c *LifecycleController) submitPreempting(ctx context.Context, lifecycle *deviceLifecycle, sub lifecycleSubmission) error {
	cmd := sub.command
	if c.commandGenerationStale(lifecycle, cmd) {
		currentGeneration := c.currentGeneration(lifecycle)
		logger.Debug("忽略过期 VoWiFi lifecycle 命令",
			"device", cmd.DeviceID,
			"kind", cmd.Kind.String(),
			"command_generation", cmd.Generation,
			"current_generation", currentGeneration,
			"reason", strings.TrimSpace(cmd.Reason))
		return nil
	}
	runCtx, cmd, clearRun := c.bindCommandRun(ctx, lifecycle, sub)
	defer clearRun()
	// Preemption requests cancellation immediately, but hardware ownership is
	// transferred only after the previous command has finished its cleanup.
	if err := lifecycle.acquireRun(runCtx, cmd); err != nil {
		return err
	}
	defer lifecycle.releaseRun()
	if err := runCtx.Err(); err != nil {
		return err
	}
	return c.runCommand(runCtx, cmd)
}

func (lifecycle *deviceLifecycle) acquireRun(ctx context.Context, cmd LifecycleCommand) error {
	if cmd.QueueTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cmd.QueueTimeout)
		defer cancel()
	}
	select {
	case lifecycle.runSlot <- struct{}{}:
		return nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			logger.Warn("VoWiFi lifecycle 命令等待前一个命令结束超时，前一个命令可能仍在等旧任务退出",
				"device", cmd.DeviceID, "kind", cmd.Kind.String(), "reason", strings.TrimSpace(cmd.Reason), "err", ctx.Err())
		} else {
			logger.Debug("VoWiFi lifecycle 命令在排队时被取消",
				"device", cmd.DeviceID, "kind", cmd.Kind.String(), "reason", strings.TrimSpace(cmd.Reason), "err", ctx.Err())
			return fmt.Errorf("%w: 设备 %s 的排队请求已被新的操作取代: %w", ErrPreviousCommandRunning, cmd.DeviceID, ctx.Err())
		}
		return fmt.Errorf("%w: 设备 %s: %w", ErrPreviousCommandRunning, cmd.DeviceID, ctx.Err())
	}
}

// ErrPreviousCommandRunning reports that the wait for the device's previous
// lifecycle command ended first; that command may still own the modem.
var ErrPreviousCommandRunning = errors.New("VoWiFi 前一个操作尚未结束")

// WaitIdle returns once no lifecycle command runs for the device, or with an
// error when ctx ends first.
func (c *LifecycleController) WaitIdle(ctx context.Context, deviceID string) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	deviceID = strings.TrimSpace(deviceID)
	lifecycle := c.device(deviceID)
	if err := lifecycle.acquireRun(ctx, LifecycleCommand{DeviceID: deviceID, Reason: "wait_idle"}); err != nil {
		return err
	}
	lifecycle.releaseRun()
	return nil
}

func (lifecycle *deviceLifecycle) releaseRun() { <-lifecycle.runSlot }

func (c *LifecycleController) runCommand(ctx context.Context, cmd LifecycleCommand) error {
	if c.TestRun != nil {
		return c.TestRun(ctx, cmd)
	}
	if cmd.Kind == LifecycleCommandRecover && c.RecoverRunForTest != nil {
		return c.RecoverRunForTest(ctx, cmd.DeviceID, cmd.Reason, cmd.OverrideEPDG)
	}
	if c.run == nil {
		return fmt.Errorf("vowifi lifecycle controller run callback is nil")
	}
	return c.run(ctx, cmd)
}

func (c *LifecycleController) NextGeneration(deviceID string) uint64 {
	if c == nil {
		return 0
	}
	trimmed := strings.TrimSpace(deviceID)
	if trimmed == "" {
		return 0
	}
	lifecycle := c.device(trimmed)
	return c.nextGeneration(lifecycle)
}

func (c *LifecycleController) CurrentGeneration(deviceID string) uint64 {
	if c == nil {
		return 0
	}
	trimmed := strings.TrimSpace(deviceID)
	if trimmed == "" {
		return 0
	}
	lifecycle := c.device(trimmed)
	return c.currentGeneration(lifecycle)
}

func (c *LifecycleController) nextGeneration(lifecycle *deviceLifecycle) uint64 {
	if c == nil || lifecycle == nil {
		return 0
	}
	lifecycle.generationMu.Lock()
	defer lifecycle.generationMu.Unlock()
	lifecycle.generation++
	return lifecycle.generation
}

func (c *LifecycleController) currentGeneration(lifecycle *deviceLifecycle) uint64 {
	if c == nil || lifecycle == nil {
		return 0
	}
	lifecycle.generationMu.Lock()
	defer lifecycle.generationMu.Unlock()
	return lifecycle.generation
}

func (c *LifecycleController) commandGenerationStale(lifecycle *deviceLifecycle, cmd LifecycleCommand) bool {
	if c == nil || lifecycle == nil || cmd.Generation == 0 {
		return false
	}
	current := c.currentGeneration(lifecycle)
	return current != 0 && cmd.Generation != current
}

func (c *LifecycleController) device(deviceID string) *deviceLifecycle {
	deviceID = strings.TrimSpace(deviceID)
	c.mu.Lock()
	defer c.mu.Unlock()
	if lifecycle := c.devices[deviceID]; lifecycle != nil {
		return lifecycle
	}
	lifecycle := &deviceLifecycle{runSlot: make(chan struct{}, 1)}
	c.devices[deviceID] = lifecycle
	return lifecycle
}
