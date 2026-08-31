package main

import (
	"log"
	"time"
)

const helperStateMaintenanceEvery = time.Minute

var afterHelperStateTick = func() {}

type helperStateSyncer interface {
	TrySyncHelperState() error
}

type helperStateTicker interface {
	C() <-chan time.Time
	Stop()
}

func startHelperStateMaintenance(
	syncer helperStateSyncer,
	newTicker func(time.Duration) helperStateTicker,
) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	ticker := newTicker(helperStateMaintenanceEvery)
	go func() {
		defer close(done)
		maintainHelperState(stop, ticker.C(), syncer.TrySyncHelperState, log.Printf)
	}()
	return func() {
		ticker.Stop()
		close(stop)
		<-done
	}
}

func maintainHelperState(
	stop <-chan struct{},
	ticks <-chan time.Time,
	syncHelperState func() error,
	logf func(string, ...any),
) {
	for {
		select {
		case <-stop:
			return
		case <-ticks:
			afterHelperStateTick()
			select {
			case <-stop:
				return
			default:
			}
			if err := syncHelperState(); err != nil {
				logf("periodic helper state sync failed; retrying next tick: %v", err)
			}
		}
	}
}
