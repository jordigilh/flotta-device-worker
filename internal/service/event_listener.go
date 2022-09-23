package service

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/coreos/go-systemd/v22/dbus"
	"github.com/project-flotta/flotta-operator/models"
	log "github.com/sirupsen/logrus"
)

const (
	EventStarted EventType = "started"
	EventStopped EventType = "stopped"

	// To avoid confusion, we match the action verb tense coming from systemd sub state, even though it is not consistent
	// The only addition to the list is the "removed" case, which we use when a service is removed
	// from the system and we receive an empty unitStatus object.

	//The service is running
	running unitSubState = "running"
	//The service is starting, probably due to a restart.
	//In this case we have to notify the observers that the service is not yet up and is loading. A subsequent 'running' event will proceed.
	start unitSubState = "start"
	//The service has been stopped, due to the container being killed. Systemd will restart the service.
	stop unitSubState = "stop"
	//The service is dead, due to the container being stopped for instance.
	dead unitSubState = "dead"
	//The service failed to start
	failed unitSubState = "failed"
	//The service has been removed, no unit state is returned from systemd
	removed unitSubState = "removed"
)

type unitSubState string

type EventType string

type Event struct {
	WorkloadName string
	Type         EventType
}

type DBusEventListener struct {
	eventCh   chan *Event
	set       *dbus.SubscriptionSet
	dbusCh    <-chan map[string]*dbus.UnitStatus
	dbusErrCh <-chan error
	lock      sync.Mutex
}

func NewDBusEventListener() *DBusEventListener {
	return &DBusEventListener{lock: sync.Mutex{}, eventCh: make(chan *Event, 1000)}
}

func (e *DBusEventListener) Init(configuration models.DeviceConfigurationMessage) error {
	log.Infof("Starting DBus event listener")
	conn, err := newDbusConnection(UserBus)
	if err != nil {
		return fmt.Errorf("error while starting event listener: %v", err)
	}
	e.set = conn.NewSubscriptionSet()
	for _, w := range configuration.Workloads {
		s, err := conn.GetUnitPropertyContext(context.Background(), DefaultServiceName(w.Name), "UnitFileState")
		if err != nil {
			return fmt.Errorf("error while retrieving workload '%s' state: %v", w.Name, err)
		}
		log.Debugf("Unit UnitFileState property for workload '%s':%s", w.Name, s.Value.String())
		v, err := strconv.Unquote(s.Value.String())
		if err != nil {
			return err
		}
		if v == "disabled" {
			log.Warnf("Service for workload '%s' is disabled", w.Name)
		}
		e.add(DefaultServiceName(w.Name))
	}
	go e.Listen()
	return nil
}

func (e *DBusEventListener) Update(configuration models.DeviceConfigurationMessage) error {
	for _, wl := range configuration.Workloads {
		svcName := DefaultServiceName(wl.Name)
		if !e.contains(svcName) {
			e.add(svcName)
		} else {
			return fmt.Errorf("unable to add systemd service '%s': there is service unit already being monitored with the same name", svcName)
		}
	}
	return nil
}

func (e *DBusEventListener) String() string {
	return "DBus event listener"
}

func (e *DBusEventListener) GetEventChannel() <-chan *Event {
	return e.eventCh
}

func (e *DBusEventListener) Listen() {
	e.dbusCh, e.dbusErrCh = e.set.Subscribe()
	for {
		select {
		case msg := <-e.dbusCh:
			for name, unit := range msg {
				log.Debugf("Captured DBus event for %s: %v+", name, unit)
				n, err := extractWorkloadName(name)
				if err != nil {
					log.Error(err)
					continue
				}
				state := translateUnitSubStatus(unit)
				log.Debugf("Systemd service for workload %s transitioned to sub state %s\n", n, state)
				switch state {
				case running:
					log.Debugf("Sending start event to observer channel for workload %s", n)
					e.eventCh <- &Event{WorkloadName: n, Type: EventStarted}
				case removed:
					log.Debugf("Service %s has been removed", name)
					e.remove(name)
					fallthrough
				case stop, dead, failed, start:
					log.Debugf("Sending stop event to observer channel for workload %s", n)
					e.eventCh <- &Event{WorkloadName: n, Type: EventStopped}
				default:
					log.Debugf("Ignoring unit sub state for service %s: %s", name, unit.SubState)
				}
			}
		case msg := <-e.dbusErrCh:
			log.Errorf("Error while parsing dbus event: %v", msg)
		}
	}

}

func (e *DBusEventListener) add(serviceName string) {
	e.lock.Lock()
	defer e.lock.Unlock()
	log.Debugf("Adding service for event listener %s", serviceName)
	e.set.Add(serviceName)
}

func (e *DBusEventListener) remove(serviceName string) {
	e.lock.Lock()
	defer e.lock.Unlock()
	log.Debugf("Removing service from event listener %s", serviceName)
	e.set.Remove(serviceName)
}

func (e *DBusEventListener) contains(serviceName string) bool {
	e.lock.Lock()
	defer e.lock.Unlock()
	return e.set.Contains(serviceName)
}

func extractWorkloadName(serviceName string) (string, error) {
	if filepath.Ext(serviceName) != ServiceSuffix {
		return "", fmt.Errorf("invalid file name or not a service %s", serviceName)
	}
	return strings.TrimSuffix(filepath.Base(serviceName), filepath.Ext(serviceName)), nil
}

func translateUnitSubStatus(unit *dbus.UnitStatus) unitSubState {
	if unit == nil {
		return removed
	}
	return unitSubState(unit.SubState)
}
