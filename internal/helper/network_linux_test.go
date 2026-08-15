//go:build linux

package helper

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestNewLinuxTapDisablesPacketInfo(t *testing.T) {
	t.Parallel()

	tap := newLinuxTap("tbx0-deadbeef", 0, 42)
	if tap.Flags&netlink.TUNTAP_NO_PI == 0 {
		t.Fatalf("tap flags = %#x, want TUNTAP_NO_PI", tap.Flags)
	}
}

func TestRequireLinuxTapNoPacketInfo(t *testing.T) {
	t.Parallel()

	if err := requireLinuxTapNoPacketInfo("tbx0-deadbeef", int64(unix.IFF_TAP|unix.IFF_NO_PI)); err != nil {
		t.Fatalf("NO_PI tap rejected: %v", err)
	}
	err := requireLinuxTapNoPacketInfo("tbx0-deadbeef", int64(unix.IFF_TAP))
	if err == nil || !strings.Contains(err.Error(), "packet-info framing") || !strings.Contains(err.Error(), "stale VM") {
		t.Fatalf("PI tap error = %v, want actionable stale-VM diagnostic", err)
	}
}

func TestListLinuxLinkStatesSkipsVanishedLink(t *testing.T) {
	t.Parallel()

	stale := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "stale0", Index: 99, Alias: "stale"}}
	live := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "live0", Index: 1, Alias: "owned"}}
	addrErr := errors.New("address dump raced with removal")
	states, err := listLinuxLinkStates(
		[]netlink.Link{stale, live},
		func(link netlink.Link, _ int) ([]netlink.Addr, error) {
			if link.Attrs().Index == stale.Attrs().Index {
				return nil, addrErr
			}
			return nil, nil
		},
		func(index int) (netlink.Link, error) {
			if index == stale.Attrs().Index {
				return nil, helperLinkNotFoundError(t)
			}
			return live, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].Name != "live0" || states[0].Alias != "owned" {
		t.Fatalf("link states = %+v, want only live0 snapshot", states)
	}
}

func TestListLinuxLinkStatesKeepsUnexpectedAddressErrors(t *testing.T) {
	t.Parallel()

	live := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "live0", Index: 1}}
	want := errors.New("netlink permission denied")
	_, err := listLinuxLinkStates(
		[]netlink.Link{live},
		func(netlink.Link, int) ([]netlink.Addr, error) { return nil, want },
		func(int) (netlink.Link, error) { return live, nil },
	)
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "dump netlink addresses for live0") {
		t.Fatalf("listLinuxLinkStates() error = %v, want wrapped address error", err)
	}
}

func helperLinkNotFoundError(t *testing.T) error {
	t.Helper()
	_, err := netlink.LinkByIndex(math.MaxInt32)
	var notFound netlink.LinkNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("LinkByIndex(max int) error = %v, want LinkNotFoundError", err)
	}
	return err
}
