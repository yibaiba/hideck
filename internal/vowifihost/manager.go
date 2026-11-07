package vowifihost

import (
	"context"
	"sync"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost"
	"github.com/iniwex5/vowifi-go/runtimehost/eventhost"
	"github.com/iniwex5/vowifi-go/runtimehost/messaging"
	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
)

type Manager struct {
	stateLocksMu   sync.Mutex
	stateLocks     map[string]*sync.Mutex
	runtimeStore   RuntimeStore
	stateHub       *StateHub
	recoverStore   *DesiredRecoverStore
	healthStore    *wifiCallingHealthStore
	lifecycle      *LifecycleController
	runtimeStart   runtimeStartFunc
	runtimeRecycle func(deviceID, reason string)
	adapter        Adapter
	voiceGateway   *voicehost.Gateway
	deliveryStore  messaging.DeliveryStore
	dispatcher     eventhost.Dispatcher

	// subscriptionRetries is handed to every runtime the manager starts, so
	// SUBSCRIBE failure backoff survives runtime restarts, recoveries and
	// Worker rebuilds; a new process starts empty (hideck#102).
	subscriptionRetries *runtimehost.SubscriptionRetries
}

func NewManager() *Manager {
	return NewManagerWithRuntimeStore(NewRuntimeStore())
}

func NewManagerWithRuntimeStore(store RuntimeStore) *Manager {
	if store == nil {
		store = NewRuntimeStore()
	}
	m := &Manager{
		runtimeStore: store,
		stateHub:     NewStateHub(),
		recoverStore: NewDesiredRecoverStore(),
		healthStore:  newWiFiCallingHealthStore(),

		subscriptionRetries: runtimehost.NewSubscriptionRetries(),
	}
	m.lifecycle = NewLifecycleController(LifecycleControllerOptions{
		IsActive: m.Active,
		Run:      m.runLifecycleCommand,
	})
	return m
}

func (m *Manager) RuntimeStore() RuntimeStore {
	if m == nil || m.runtimeStore == nil {
		return NewRuntimeStore()
	}
	return m.runtimeStore
}

func (m *Manager) SubscribeState(deviceID string) (<-chan runtimehost.State, func()) {
	return m.stateNotifications().Subscribe(deviceID)
}

func (m *Manager) BroadcastState(deviceID string) {
	state, _ := m.State(deviceID)
	m.stateNotifications().Broadcast(deviceID, state)
}

func (m *Manager) SubscriberCount(deviceID string) int {
	return m.stateNotifications().SubscriberCount(deviceID)
}

func (m *Manager) RecordStartupState(deviceID string, state runtimehost.State) bool {
	if !m.RuntimeStore().RecordStartupState(deviceID, state) {
		return false
	}
	m.observeWiFiCallingHealth(deviceID, state)
	m.BroadcastState(deviceID)
	return true
}

func (m *Manager) observeWiFiCallingHealth(deviceID string, state runtimehost.State) {
	if m != nil && m.healthStore != nil {
		m.healthStore.Observe(deviceID, state)
	}
}

func (m *Manager) EndWiFiCallingHealth(deviceID, reason string) {
	if m != nil && m.healthStore != nil {
		m.healthStore.End(deviceID, reason, time.Now())
	}
}

func (m *Manager) BeginWiFiCallingHealth(deviceID string) {
	if m != nil && m.healthStore != nil {
		m.healthStore.Begin(deviceID, time.Now())
	}
}

func (m *Manager) FailWiFiCallingHealthStart(deviceID string, err error) {
	if m == nil || m.healthStore == nil {
		return
	}
	reason := ""
	if err != nil {
		reason = err.Error()
	}
	m.healthStore.FailStart(deviceID, reason, time.Now())
}

func (m *Manager) WiFiCallingHealth(deviceID string) (WiFiCallingHealthSnapshot, bool) {
	if m == nil || m.healthStore == nil {
		return WiFiCallingHealthSnapshot{}, false
	}
	return m.healthStore.Snapshot(deviceID, time.Now())
}

func (m *Manager) ClearStartupState(deviceID string) bool {
	return m.RuntimeStore().ClearStartupState(deviceID)
}

func (m *Manager) ConfigureRuntimeDependencies(vg *voicehost.Gateway, ds messaging.DeliveryStore, ed eventhost.Dispatcher) {
	if m == nil {
		return
	}
	m.voiceGateway = vg
	m.deliveryStore = ds
	m.dispatcher = ed
}

func (m *Manager) ConfigureRuntimeRecycleHandler(handler func(deviceID, reason string)) {
	if m != nil {
		m.runtimeRecycle = handler
	}
}

func (m *Manager) requestRuntimeRecycle(deviceID, reason string) {
	if m != nil && m.runtimeRecycle != nil {
		m.runtimeRecycle(deviceID, reason)
	}
}

func (m *Manager) ClearStartupStateAndBroadcast(deviceID string) {
	m.ClearStartupState(deviceID)
	m.BroadcastState(deviceID)
}

func (m *Manager) stateNotifications() *StateHub {
	if m == nil || m.stateHub == nil {
		return NewStateHub()
	}
	return m.stateHub
}

func (m *Manager) desiredRecoverStore() *DesiredRecoverStore {
	if m == nil || m.recoverStore == nil {
		return NewDesiredRecoverStore()
	}
	return m.recoverStore
}

func (m *Manager) ConfigureLifecycle(options LifecycleControllerOptions) {
	if m == nil {
		return
	}
	if options.IsActive == nil {
		options.IsActive = m.Active
	}
	if options.Run == nil {
		options.Run = m.runLifecycleCommand
	}
	m.lifecycle = NewLifecycleController(options)
}

func (m *Manager) SubmitLifecycle(ctx context.Context, cmd LifecycleCommand) error {
	return m.lifecycleController().Submit(ctx, cmd)
}

func (m *Manager) NextLifecycleGeneration(deviceID string) uint64 {
	return m.lifecycleController().NextGeneration(deviceID)
}

func (m *Manager) CurrentLifecycleGeneration(deviceID string) uint64 {
	return m.lifecycleController().CurrentGeneration(deviceID)
}

func (m *Manager) SetLifecycleRunForTest(fn func(context.Context, LifecycleCommand) error) {
	m.lifecycleController().SetRunForTest(fn)
}

func (m *Manager) SetLifecycleRecoverRunForTest(fn func(context.Context, string, string, string) error) {
	m.lifecycleController().SetRecoverRunForTest(fn)
}

func (m *Manager) LifecycleControllerForTest() *LifecycleController {
	return m.lifecycleController()
}

func (m *Manager) lifecycleController() *LifecycleController {
	if m == nil || m.lifecycle == nil {
		if m == nil {
			return NewLifecycleController()
		}
		m.ConfigureLifecycle(LifecycleControllerOptions{})
	}
	return m.lifecycle
}
