package event

import (
	"sync"
)

// EventName represents the type of event
type EventName string

const (
	UserCreated          EventName = "user_created"
	HostCreated          EventName = "host_created"
	HostApproved         EventName = "host_approved"
	HostRejected         EventName = "host_rejected"
	EventCreated         EventName = "event_created"
	EventDeleted         EventName = "event_deleted"
	EventCancelled       EventName = "event_cancelled"
	BookingCreated       EventName = "booking_created"
	BookingConfirmed     EventName = "booking_confirmed"
	BookingCancelled     EventName = "booking_cancelled"
	BookingStatusChanged EventName = "booking_status_changed"
	PayoutCompleted      EventName = "payout_completed"
	PayoutFailed         EventName = "payout_failed"
	PaymentCreated       EventName = "payment_created"
)

// Observer (Subscriber) interface
type Observer interface {
	OnNotify(event EventName, data interface{})
}

// Dispatcher manages event subscriptions and notifications (Observer Pattern / Singleton)
type Dispatcher struct {
	observers map[EventName][]Observer
	mu        sync.RWMutex
}

var instance *Dispatcher
var once sync.Once

// GetDispatcher returns the singleton instance of the event dispatcher
func GetDispatcher() *Dispatcher {
	once.Do(func() {
		instance = &Dispatcher{
			observers: make(map[EventName][]Observer),
		}
	})
	return instance
}

// Subscribe adds an observer for a specific event
func (d *Dispatcher) Subscribe(event EventName, observer Observer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.observers[event] = append(d.observers[event], observer)
}

// Publish notifies all observers of an event
func (d *Dispatcher) Publish(event EventName, data interface{}) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if observers, found := d.observers[event]; found {
		for _, observer := range observers {
			// Trigger observer logic synchronously or asynchronously depending on requirement
			// Here we run it directly, but could launch a goroutine if needed
			go observer.OnNotify(event, data)
		}
	}
}
