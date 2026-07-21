package main

import (
	"errors"
	"testing"
	"time"
)

func TestCheckHostNetworking(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		resolver      string
		resolverErr   error
		forwarding    string
		forwardingErr error
		want          hostNetworkingDrift
	}{
		{
			name:       "clean host",
			resolver:   "nameserver 127.0.0.1\nport 53535\n",
			forwarding: "1\n",
		},
		{
			name:       "resolver content drift",
			resolver:   "nameserver 8.8.8.8\n",
			forwarding: "1\n",
			want:       hostNetworkingDrift{dns: true},
		},
		{
			name:        "resolver missing",
			resolverErr: errors.New("not found"),
			forwarding:  "1\n",
			want:        hostNetworkingDrift{dns: true},
		},
		{
			name:       "forwarding disabled",
			resolver:   "nameserver 127.0.0.1\nport 53535\n",
			forwarding: "0\n",
			want:       hostNetworkingDrift{forwarding: true},
		},
		{
			name:          "both checks fail",
			resolverErr:   errors.New("permission denied"),
			forwardingErr: errors.New("sysctl failed"),
			want:          hostNetworkingDrift{dns: true, forwarding: true},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := checkHostNetworking(53535,
				func(path string) ([]byte, error) {
					if path != resolverPath {
						t.Fatalf("read path = %q, want %q", path, resolverPath)
					}
					return []byte(test.resolver), test.resolverErr
				},
				func(name string, args ...string) ([]byte, error) {
					if name != "/usr/sbin/sysctl" {
						t.Fatalf("command = %q, want /usr/sbin/sysctl", name)
					}
					if len(args) != 2 || args[0] != "-n" || args[1] != "net.inet.ip.forwarding" {
						t.Fatalf("arguments = %q", args)
					}
					return []byte(test.forwarding), test.forwardingErr
				},
			)
			if got != test.want {
				t.Fatalf("checkHostNetworking() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestReassertHostNetworkingRepairsOnlyDrift(t *testing.T) {
	t.Parallel()

	client := &fakeHostNetworkingClient{}
	err := reassertHostNetworking(hostNetworkingDrift{dns: true}, func() (hostNetworkingClient, error) {
		return client, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.installDNSCalls != 1 || client.enableForwardingCalls != 0 || client.closeCalls != 1 {
		t.Fatalf("helper calls = %+v", client)
	}
}

func TestMaintainHostNetworkingRetriesAfterHelperUnavailable(t *testing.T) {
	client := &fakeHostNetworkingClient{}
	ticks := make(chan time.Time)
	stop := make(chan struct{})
	done := make(chan struct{})
	connectCalls := make(chan int, 2)
	calls := 0
	go func() {
		defer close(done)
		maintainHostNetworking(
			stop,
			ticks,
			func(string) ([]byte, error) { return nil, errors.New("resolver missing") },
			func(string, ...string) ([]byte, error) { return []byte("1\n"), nil },
			func() (hostNetworkingClient, error) {
				calls++
				connectCalls <- calls
				if calls == 1 {
					return nil, errors.New("helper restarting")
				}
				return client, nil
			},
		)
	}()

	ticks <- time.Now()
	if got := <-connectCalls; got != 1 {
		t.Fatalf("first connect call = %d", got)
	}
	ticks <- time.Now()
	if got := <-connectCalls; got != 2 {
		t.Fatalf("second connect call = %d", got)
	}
	close(stop)
	<-done

	if client.installDNSCalls != 1 {
		t.Fatalf("InstallDNS calls = %d, want 1 after retry", client.installDNSCalls)
	}
}

type fakeHostNetworkingClient struct {
	installDNSCalls       int
	enableForwardingCalls int
	closeCalls            int
}

func (c *fakeHostNetworkingClient) InstallDNS(int) error {
	c.installDNSCalls++
	return nil
}

func (c *fakeHostNetworkingClient) EnableForwarding() error {
	c.enableForwardingCalls++
	return nil
}

func (c *fakeHostNetworkingClient) Close() error {
	c.closeCalls++
	return nil
}
