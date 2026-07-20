package daemon

import (
	"errors"
	"testing"
)

func TestWithClusterStoppedOrdersLifecycle(t *testing.T) {
	var events []string
	err := withClusterStopped(true,
		func() error { events = append(events, "stop"); return nil },
		func() error { events = append(events, "start"); return nil },
		func() error { events = append(events, "body"); return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"stop", "body", "start"}
	if len(events) != 3 || events[0] != want[0] || events[1] != want[1] || events[2] != want[2] {
		t.Errorf("lifecycle order = %v, want %v", events, want)
	}
}

func TestWithClusterStoppedSkipsLifecycleWhenNotRunning(t *testing.T) {
	var events []string
	_ = withClusterStopped(false,
		func() error { events = append(events, "stop"); return nil },
		func() error { events = append(events, "start"); return nil },
		func() error { events = append(events, "body"); return nil },
	)
	if len(events) != 1 || events[0] != "body" {
		t.Errorf("stopped cluster should only run body, got %v", events)
	}
}

func TestWithClusterStoppedRestartsAfterBodyFailure(t *testing.T) {
	var started bool
	err := withClusterStopped(true,
		func() error { return nil },
		func() error { started = true; return nil },
		func() error { return errors.New("clone failed") },
	)
	if err == nil {
		t.Fatal("body error should surface")
	}
	if !started {
		t.Error("cluster must be restarted even when the body fails")
	}
}
