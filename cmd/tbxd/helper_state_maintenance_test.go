package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeHelperStateSyncer struct {
	called chan struct{}
}

func (s *fakeHelperStateSyncer) TrySyncHelperState() error {
	s.called <- struct{}{}
	return nil
}

type fakeHelperStateTicker struct {
	ticks   chan time.Time
	stopped chan struct{}
}

func (t *fakeHelperStateTicker) C() <-chan time.Time { return t.ticks }
func (t *fakeHelperStateTicker) Stop()               { close(t.stopped) }

func TestStartHelperStateMaintenanceUsesOneMinuteTicker(t *testing.T) {
	ticker := &fakeHelperStateTicker{ticks: make(chan time.Time), stopped: make(chan struct{})}
	syncer := &fakeHelperStateSyncer{called: make(chan struct{}, 1)}
	var interval time.Duration
	stop := startHelperStateMaintenance(syncer, func(got time.Duration) helperStateTicker {
		interval = got
		return ticker
	})

	if interval != time.Minute {
		t.Fatalf("maintenance interval = %s, want %s", interval, time.Minute)
	}
	ticker.ticks <- time.Time{}
	select {
	case <-syncer.called:
	case <-time.After(time.Second):
		t.Fatal("started maintenance loop did not sync on its ticker")
	}
	stop()
	select {
	case <-ticker.stopped:
	default:
		t.Fatal("maintenance stop did not stop its ticker")
	}
}

func TestMaintainHelperStateSyncsExactlyOncePerTick(t *testing.T) {
	stop := make(chan struct{})
	ticks := make(chan time.Time)
	done := make(chan struct{})
	called := make(chan struct{}, 2)
	go func() {
		defer close(done)
		maintainHelperState(stop, ticks, func() error {
			called <- struct{}{}
			return nil
		}, func(string, ...any) {})
	}()

	ticks <- time.Time{}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("maintenance tick did not sync helper state")
	}
	select {
	case <-called:
		t.Fatal("one maintenance tick performed more than one helper sync")
	case <-time.After(25 * time.Millisecond):
	}
	close(stop)
	<-done
}

func TestMaintainHelperStateLogsFailureAndRetriesNextTick(t *testing.T) {
	stop := make(chan struct{})
	ticks := make(chan time.Time)
	done := make(chan struct{})
	called := make(chan int, 2)
	var logMu sync.Mutex
	var logs []string
	attempt := 0
	go func() {
		defer close(done)
		maintainHelperState(stop, ticks, func() error {
			attempt++
			called <- attempt
			if attempt == 1 {
				return errors.New("helper unavailable")
			}
			return nil
		}, func(format string, args ...any) {
			logMu.Lock()
			logs = append(logs, fmt.Sprintf(format, args...))
			logMu.Unlock()
		})
	}()

	ticks <- time.Time{}
	if got := <-called; got != 1 {
		t.Fatalf("first tick attempt = %d, want 1", got)
	}
	ticks <- time.Time{}
	if got := <-called; got != 2 {
		t.Fatalf("second tick attempt = %d, want retry 2", got)
	}
	close(stop)
	<-done

	logMu.Lock()
	defer logMu.Unlock()
	if len(logs) != 1 || !strings.Contains(logs[0], "helper unavailable") {
		t.Fatalf("logs = %q, want one helper-unavailable failure", logs)
	}
}

func TestMaintainHelperStateSkipsQueuedTickAfterStop(t *testing.T) {
	originalAfterTick := afterHelperStateTick
	t.Cleanup(func() { afterHelperStateTick = originalAfterTick })

	stop := make(chan struct{})
	ticks := make(chan time.Time, 1)
	done := make(chan struct{})
	called := make(chan struct{}, 1)
	afterHelperStateTick = func() { close(stop) }
	go func() {
		defer close(done)
		maintainHelperState(stop, ticks, func() error {
			called <- struct{}{}
			return nil
		}, func(string, ...any) {})
	}()

	ticks <- time.Time{}
	<-done
	select {
	case <-called:
		t.Fatal("queued tick still synced helper state after stop")
	default:
	}
}
