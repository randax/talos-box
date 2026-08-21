package helper

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/randax/talos-box/internal/bgp"
)

func TestBGPRejectsInvalidArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		op      string
		args    string
		wantErr string
	}{
		{name: "enable requires cluster", op: "bgp.enable", args: `{"subnetIndex":7,"localASN":64512,"peerASN":64600}`, wantErr: "cluster is required"},
		{name: "disable requires cluster", op: "bgp.disable", args: `{}`, wantErr: "cluster is required"},
		{name: "subnet is required", op: "bgp.enable", args: `{"cluster":"demo","localASN":64512,"peerASN":64600}`, wantErr: "subnetIndex is required"},
		{name: "negative subnet", op: "bgp.enable", args: `{"cluster":"demo","subnetIndex":-1,"localASN":64512,"peerASN":64600}`, wantErr: "outside 0..255"},
		{name: "subnet above IPv4 octet", op: "bgp.enable", args: `{"cluster":"demo","subnetIndex":256,"localASN":64512,"peerASN":64600}`, wantErr: "outside 0..255"},
		{name: "local ASN is required", op: "bgp.enable", args: `{"cluster":"demo","subnetIndex":7,"peerASN":64600}`, wantErr: "ASNs must be non-zero"},
		{name: "peer ASN is required", op: "bgp.enable", args: `{"cluster":"demo","subnetIndex":7,"localASN":64512}`, wantErr: "ASNs must be non-zero"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reply := NewServer(nil).dispatch(Request{Op: test.op, Args: json.RawMessage(test.args)})
			if reply.response.OK || !strings.Contains(reply.response.Error, test.wantErr) {
				t.Fatalf("dispatch(%s) response = %+v, want error containing %q", test.op, reply.response, test.wantErr)
			}
		})
	}
}

func TestBGPStatusReportsObservedSpeakerOwnership(t *testing.T) {
	server := NewServer(nil)
	announced := []bgp.Route{
		{Prefix: "172.30.7.200/32", Nexthop: "172.30.7.2"},
		{Prefix: "172.30.7.201/32", Nexthop: "172.30.7.3"},
	}
	server.speakers = map[string]bgpSpeaker{
		"demo":   &reportingSpeaker{routes: announced},
		"silent": &shutdownSpeaker{},
	}
	for _, test := range []struct {
		name       string
		args       string
		active     bool
		wantRoutes []BGPRoute
	}{
		{
			name:   "active speaker reports its announced routes",
			args:   `{"cluster":"demo"}`,
			active: true,
			wantRoutes: []BGPRoute{
				{Prefix: "172.30.7.200/32", Nexthop: "172.30.7.2"},
				{Prefix: "172.30.7.201/32", Nexthop: "172.30.7.3"},
			},
		},
		{name: "speaker that cannot report routes", args: `{"cluster":"silent"}`, active: true},
		{name: "no speaker", args: `{"cluster":"missing"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			reply := server.dispatch(Request{Op: "bgp.status", Args: json.RawMessage(test.args)})
			if !reply.response.OK {
				t.Fatalf("bgp.status response = %+v", reply.response)
			}
			var data BGPState
			if err := json.Unmarshal(reply.response.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data.Active != test.active {
				t.Fatalf("bgp.status active = %t, want %t", data.Active, test.active)
			}
			if !reflect.DeepEqual(data.Routes, test.wantRoutes) {
				t.Fatalf("bgp.status routes = %v, want %v", data.Routes, test.wantRoutes)
			}
		})
	}
}

type shutdownSpeaker struct{ stops int }

func (s *shutdownSpeaker) Stop() { s.stops++ }

// reportingSpeaker stands in for the real *bgp.Speaker, which reports the
// routes it has installed in the host FIB.
type reportingSpeaker struct {
	shutdownSpeaker
	routes []bgp.Route
}

func (s *reportingSpeaker) Routes() []bgp.Route { return s.routes }

func TestShutdownStopsEveryBGPSpeaker(t *testing.T) {
	server := NewServer(nil)
	first, second := &shutdownSpeaker{}, &shutdownSpeaker{}
	server.speakers = map[string]bgpSpeaker{"first": first, "second": second}
	if err := server.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if first.stops != 1 || second.stops != 1 || len(server.speakers) != 0 {
		t.Fatalf("BGP shutdown = first:%d second:%d remaining:%d", first.stops, second.stops, len(server.speakers))
	}
}

func TestListenUnixSocketPreservesNonCollisionBindError(t *testing.T) {
	t.Parallel()

	removeCalled := false
	_, err := listenUnixSocket(
		"/restricted/tbx-helper.sock",
		func(string, string) (net.Listener, error) { return nil, os.ErrPermission },
		func(string, string, time.Duration) (net.Conn, error) {
			t.Fatal("dial called for permission error")
			return nil, nil
		},
		func(string) error { removeCalled = true; return nil },
	)
	if !errors.Is(err, os.ErrPermission) || removeCalled {
		t.Fatalf("listenUnixSocket() error = %v, removeCalled = %t; want original permission error only", err, removeCalled)
	}
}

func TestListenUnixSocketRetainsBindErrorWhenStaleRemovalFails(t *testing.T) {
	t.Parallel()

	removeErr := errors.New("remove denied")
	_, err := listenUnixSocket(
		"/run/tbx-helper.sock",
		func(string, string) (net.Listener, error) { return nil, unix.EADDRINUSE },
		func(string, string, time.Duration) (net.Conn, error) { return nil, errors.New("connection refused") },
		func(string) error { return removeErr },
	)
	if !errors.Is(err, unix.EADDRINUSE) || !errors.Is(err, removeErr) {
		t.Fatalf("listenUnixSocket() error = %v, want original bind and cleanup errors", err)
	}
}

func TestIsAuthorizedUID(t *testing.T) {
	t.Parallel()

	allowedUID := uint32(501)
	tests := []struct {
		name       string
		uid        uint32
		allowedUID *uint32
		allowAny   bool
		want       bool
	}{
		{name: "allowed uid", uid: 501, allowedUID: &allowedUID, want: true},
		{name: "root", uid: 0, allowedUID: &allowedUID, want: true},
		{name: "other uid", uid: 502, allowedUID: &allowedUID, want: false},
		{name: "unset allows root", uid: 0, want: true},
		{name: "unset rejects user", uid: 501, want: false},
		{name: "socket-admitted group peer", uid: 501, allowAny: true, want: true},
		{name: "explicit uid remains authoritative for activated socket", uid: 502, allowedUID: &allowedUID, allowAny: true, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := isAuthorizedUID(test.uid, test.allowedUID, test.allowAny); got != test.want {
				t.Fatalf("isAuthorizedUID(%d) = %t, want %t", test.uid, got, test.want)
			}
		})
	}
}
