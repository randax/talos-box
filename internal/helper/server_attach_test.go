package helper

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDetachKeepsAttachmentOnStopFailure(t *testing.T) {
	server := NewServer(nil, nil)
	key := attachmentKey{owner: 0, cluster: "demo", node: "cp-1"}
	server.attachments[key] = testPlatformAttachment(42, func(fd int) error {
		if fd != 42 {
			t.Fatalf("stop attachment fd = %d, want 42", fd)
		}
		return wrapVMNetStopError(errors.New("retry later"), true)
	})

	err := server.detach(0, json.RawMessage(`{"cluster":"demo","node":"cp-1"}`))
	if err == nil {
		t.Fatal("detach() error = nil, want failure")
	}
	if _, ok := server.attachments[key]; !ok {
		t.Fatal("attachment mapping was removed on stop failure")
	}
}

func TestAttachCleanupDropsAttachmentOnRetainedStopFailure(t *testing.T) {
	server := NewServer(nil, nil)
	server.dhcp = &recordingDHCPManager{subnets: server.attachedSubnetIndexes}
	startCalls := 0
	stopCalls := make(map[int]int)

	originalStart := startInterface
	startInterface = func(_ []int, subnet int, _, _ string) (*platformAttachment, error) {
		if subnet != 7 {
			t.Fatalf("startInterface subnet = %d, want 7", subnet)
		}
		startCalls++
		return testPlatformAttachment(98+startCalls, func(fd int) error {
			stopCalls[fd]++
			switch fd {
			case 99:
				if stopCalls[fd] == 1 {
					return wrapVMNetStopError(errors.New("retry later"), true)
				}
				return nil
			case 100:
				return nil
			default:
				t.Fatalf("unexpected stop attachment fd = %d", fd)
				return nil
			}
		}), nil
	}
	t.Cleanup(func() {
		startInterface = originalStart
	})

	_, fd, cleanup, err := server.attach(0, json.RawMessage(`{"cluster":"demo","subnetIndex":7,"node":"cp-1"}`))
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	if fd != 99 {
		t.Fatalf("attach() fd = %d, want 99", fd)
	}
	cleanup()

	key := attachmentKey{owner: 0, cluster: "demo", node: "cp-1"}
	if _, ok := server.attachments[key]; ok {
		t.Fatal("attachment mapping retained after failed response cleanup")
	}
	if _, ok := server.pendingStops[99]; !ok {
		t.Fatal("retained stop was not recorded for shutdown retry")
	}

	_, retryFD, retryCleanup, err := server.attach(0, json.RawMessage(`{"cluster":"demo","subnetIndex":7,"node":"cp-1"}`))
	if err != nil {
		t.Fatalf("retry attach() error = %v", err)
	}
	if retryFD != 100 {
		t.Fatalf("retry attach fd = %d, want 100", retryFD)
	}

	retryCleanup()
	if err := server.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if got := stopCalls[99]; got != 2 {
		t.Fatalf("stopInterface calls for retained fd = %d, want 2", got)
	}
	if len(server.pendingStops) != 0 {
		t.Fatalf("pending stops after successful shutdown retry = %v, want empty", server.pendingStops)
	}
}

func TestAttachRollsBackInterfaceWhenDHCPConvergenceFails(t *testing.T) {
	server := NewServer(nil, nil)
	convergeErr := errors.New("DHCP unavailable")
	server.dhcp = &testDHCPManager{convergeErr: convergeErr}
	stopCalls := 0

	originalStart := startInterface
	startInterface = func([]int, int, string, string) (*platformAttachment, error) {
		return testPlatformAttachment(77, func(int) error {
			stopCalls++
			return nil
		}), nil
	}
	t.Cleanup(func() { startInterface = originalStart })

	_, _, _, err := server.attach(0, json.RawMessage(`{"cluster":"demo","subnetIndex":7,"node":"cp-1"}`))
	if !errors.Is(err, convergeErr) {
		t.Fatalf("attach() error = %v, want %v", err, convergeErr)
	}
	if stopCalls != 1 {
		t.Fatalf("attachment stop calls = %d, want 1", stopCalls)
	}
	if len(server.attachments) != 0 {
		t.Fatalf("attachments = %v, want empty", server.attachments)
	}
}

func TestShutdownRetriesRetainedAttachmentStops(t *testing.T) {
	server := NewServer(nil, nil)
	key := attachmentKey{owner: 0, cluster: "demo", node: "cp-1"}
	stopCalls := 0
	server.attachments[key] = testPlatformAttachment(42, func(fd int) error {
		if fd != 42 {
			t.Fatalf("stop attachment fd = %d, want 42", fd)
		}
		stopCalls++
		if stopCalls == 1 {
			return wrapVMNetStopError(errors.New("retry later"), true)
		}
		return nil
	})

	if err := server.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if stopCalls != 2 {
		t.Fatalf("stopInterface calls = %d, want 2", stopCalls)
	}
	if _, ok := server.attachments[key]; ok {
		t.Fatal("attachment retained after successful shutdown retry")
	}
}

func TestShutdownClosesDHCPService(t *testing.T) {
	server := NewServer(nil, nil)
	closeErr := errors.New("close DHCP")
	server.dhcp = &testDHCPManager{closeErr: closeErr}

	if err := server.Shutdown(); !errors.Is(err, closeErr) {
		t.Fatalf("Shutdown() error = %v, want %v", err, closeErr)
	}
}

func TestShutdownBoundsRetainedStopRetries(t *testing.T) {
	server := NewServer(nil, nil)
	stopCalls := 0
	stopErr := errors.New("retry later")
	server.pendingStops[42] = testPlatformAttachment(42, func(fd int) error {
		if fd != 42 {
			t.Fatalf("stop attachment fd = %d, want 42", fd)
		}
		stopCalls++
		return wrapVMNetStopError(stopErr, true)
	})

	err := server.Shutdown()
	if !errors.Is(err, stopErr) {
		t.Fatalf("Shutdown() error = %v, want %v", err, stopErr)
	}
	if stopCalls != shutdownStopMaxAttempts {
		t.Fatalf("stopInterface calls = %d, want %d", stopCalls, shutdownStopMaxAttempts)
	}
	if _, ok := server.pendingStops[42]; !ok {
		t.Fatal("retained stop removed after exhausting shutdown retries")
	}
}

func TestDetachDropsAttachmentOnTerminalStopFailure(t *testing.T) {
	server := NewServer(nil, nil)
	key := attachmentKey{owner: 0, cluster: "demo", node: "cp-1"}
	server.attachments[key] = testPlatformAttachment(42, func(fd int) error {
		if fd != 42 {
			t.Fatalf("stop attachment fd = %d, want 42", fd)
		}
		return wrapVMNetStopError(errors.New("released"), false)
	})

	err := server.detach(0, json.RawMessage(`{"cluster":"demo","node":"cp-1"}`))
	if err == nil {
		t.Fatal("detach() error = nil, want failure")
	}
	if _, ok := server.attachments[key]; ok {
		t.Fatal("attachment mapping was retained after terminal stop failure")
	}
}

func testPlatformAttachment(fd int, stop func(int) error) *platformAttachment {
	return &platformAttachment{
		Kind: AttachmentDatagramFD,
		FD:   fd,
		stop: func() error {
			return stop(fd)
		},
	}
}

type testDHCPManager struct {
	convergeErr error
	closeErr    error
	released    []int
}

func (m *testDHCPManager) Converge() error { return m.convergeErr }

func (m *testDHCPManager) Release(subnetIndex int) error {
	m.released = append(m.released, subnetIndex)
	return nil
}

func (m *testDHCPManager) Close() error { return m.closeErr }

func TestTeardownRemovesTheSubnetBridge(t *testing.T) {
	server := NewServer(nil, nil)
	calls := 0

	originalTeardown := teardownSubnet
	teardownSubnet = func(subnetIndex int) (bool, error) {
		if subnetIndex != 7 {
			t.Fatalf("teardownSubnet subnet = %d, want 7", subnetIndex)
		}
		calls++
		return true, nil
	}
	t.Cleanup(func() { teardownSubnet = originalTeardown })

	reply := server.dispatch(Request{Op: "net.teardown", Args: json.RawMessage(`{"subnetIndex":7}`)})
	if !reply.response.OK {
		t.Fatalf("net.teardown response = %+v", reply.response)
	}
	var data struct {
		Removed bool `json:"removed"`
	}
	if err := json.Unmarshal(reply.response.Data, &data); err != nil {
		t.Fatal(err)
	}
	if !data.Removed {
		t.Fatal("net.teardown removed = false, want true")
	}
	if calls != 1 {
		t.Fatalf("teardownSubnet calls = %d, want 1", calls)
	}
}

func TestTeardownReportsAnAbsentBridgeAsSuccess(t *testing.T) {
	server := NewServer(nil, nil)

	originalTeardown := teardownSubnet
	teardownSubnet = func(int) (bool, error) { return false, nil }
	t.Cleanup(func() { teardownSubnet = originalTeardown })

	reply := server.dispatch(Request{Op: "net.teardown", Args: json.RawMessage(`{"subnetIndex":0}`)})
	if !reply.response.OK {
		t.Fatalf("net.teardown response = %+v", reply.response)
	}
	if string(reply.response.Data) != `{"removed":false}` {
		t.Fatalf("net.teardown data = %s, want {\"removed\":false}", reply.response.Data)
	}
}

func TestTeardownReleasesTheSubnetDHCPServerOnlyWhenTheBridgeIsGone(t *testing.T) {
	for _, test := range []struct {
		name         string
		teardown     func(int) (bool, error)
		wantOK       bool
		wantReleased []int
	}{
		{
			name:         "bridge removed",
			teardown:     func(int) (bool, error) { return true, nil },
			wantOK:       true,
			wantReleased: []int{3},
		},
		{
			name:         "bridge already absent",
			teardown:     func(int) (bool, error) { return false, nil },
			wantOK:       true,
			wantReleased: []int{3},
		},
		{
			name: "teardown refused",
			teardown: func(int) (bool, error) {
				return false, errors.New("bridge br-tbx3 still has tbx3-cp-1 attached; stop the VM before removing it")
			},
			wantOK:       false,
			wantReleased: nil,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			originalTeardown := teardownSubnet
			teardownSubnet = test.teardown
			t.Cleanup(func() { teardownSubnet = originalTeardown })

			server := NewServer(nil, nil)
			dhcp := &testDHCPManager{}
			server.dhcp = dhcp

			reply := server.dispatch(Request{Op: "net.teardown", Args: json.RawMessage(`{"subnetIndex":3}`)})
			if reply.response.OK != test.wantOK {
				t.Fatalf("net.teardown response = %+v, want OK = %t", reply.response, test.wantOK)
			}
			if len(dhcp.released) != len(test.wantReleased) {
				t.Fatalf("released subnets = %v, want %v", dhcp.released, test.wantReleased)
			}
			for i, subnetIndex := range test.wantReleased {
				if dhcp.released[i] != subnetIndex {
					t.Fatalf("released subnets = %v, want %v", dhcp.released, test.wantReleased)
				}
			}
		})
	}
}

func TestTeardownRejectsInvalidArguments(t *testing.T) {
	originalTeardown := teardownSubnet
	teardownSubnet = func(int) (bool, error) {
		t.Fatal("teardownSubnet was called for invalid arguments")
		return false, nil
	}
	t.Cleanup(func() { teardownSubnet = originalTeardown })

	for _, test := range []struct {
		name    string
		args    string
		wantErr string
	}{
		{name: "subnet is required", args: `{}`, wantErr: "subnetIndex is required"},
		{name: "negative subnet", args: `{"subnetIndex":-1}`, wantErr: "outside 0..255"},
		{name: "subnet above IPv4 octet", args: `{"subnetIndex":256}`, wantErr: "outside 0..255"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reply := NewServer(nil, nil).dispatch(Request{Op: "net.teardown", Args: json.RawMessage(test.args)})
			if reply.response.OK || !strings.Contains(reply.response.Error, test.wantErr) {
				t.Fatalf("net.teardown response = %+v, want error containing %q", reply.response, test.wantErr)
			}
		})
	}
}
