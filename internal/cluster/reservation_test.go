package cluster

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestNewAssignsStableReservations(t *testing.T) {
	t.Parallel()

	first, err := New("first", 7, 2, 1, NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := New("second", 8, 1, 1, NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}

	if got, want := []string{first.Nodes[0].IP, first.Nodes[1].IP, first.Nodes[2].IP}, []string{"172.30.7.2", "172.30.7.3", "172.30.7.4"}; !equalStrings(got, want) {
		t.Fatalf("first cluster reservations = %v, want %v", got, want)
	}
	if got, want := []string{second.Nodes[0].IP, second.Nodes[1].IP}, []string{"172.30.8.2", "172.30.8.3"}; !equalStrings(got, want) {
		t.Fatalf("second cluster reservations = %v, want %v", got, want)
	}
}

func TestAddNodeReusesLowestFreeReservation(t *testing.T) {
	t.Parallel()

	item, err := New("demo", 3, 1, 2, NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveNode(&item, "demo-worker-1"); err != nil {
		t.Fatal(err)
	}
	added, err := AddNode(&item, RoleWorker, "replacement")
	if err != nil {
		t.Fatal(err)
	}
	if added.IP != "172.30.3.3" {
		t.Fatalf("replacement IP = %q, want 172.30.3.3", added.IP)
	}
}

func TestReservationTableLookupAndValidation(t *testing.T) {
	t.Parallel()

	item, err := New("demo", 3, 1, 1, NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	table, err := NewReservationTable([]Cluster{item})
	if err != nil {
		t.Fatal(err)
	}
	if got := table.LookupIP(item.Nodes[1].MAC); !got.Equal(net.IPv4(172, 30, 3, 3)) {
		t.Fatalf("LookupIP() = %v, want 172.30.3.3", got)
	}
	if got := table.LookupIP("52:54:00:ff:ff:ff"); got != nil {
		t.Fatalf("unknown LookupIP() = %v, want nil", got)
	}
	if got := table.LookupIP("not-a-mac"); got != nil {
		t.Fatalf("invalid LookupIP() = %v, want nil", got)
	}

	duplicateIP := item
	duplicateIP.Nodes = append([]Node(nil), item.Nodes...)
	duplicateIP.Nodes[1].IP = duplicateIP.Nodes[0].IP
	if _, err := NewReservationTable([]Cluster{duplicateIP}); err == nil {
		t.Fatal("NewReservationTable() accepted duplicate IP")
	}

	wrongSubnet := item
	wrongSubnet.Nodes = append([]Node(nil), item.Nodes...)
	wrongSubnet.Nodes[0].IP = "172.30.4.2"
	if _, err := NewReservationTable([]Cluster{wrongSubnet}); err == nil {
		t.Fatal("NewReservationTable() accepted reservation outside cluster subnet")
	}
}

func TestLookupReservedIPRequiresMatchingSubnet(t *testing.T) {
	t.Parallel()

	item, err := New("demo", 3, 1, 0, NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	if got := lookupReservedIP([]Cluster{item}, item.Nodes[0].MAC, 3); got != "172.30.3.2" {
		t.Fatalf("lookupReservedIP() = %q, want 172.30.3.2", got)
	}
	if got := lookupReservedIP([]Cluster{item}, item.Nodes[0].MAC, 4); got != "" {
		t.Fatalf("wrong-subnet lookupReservedIP() = %q, want empty", got)
	}
}

func TestLoadBackfillsLegacyReservations(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	item, err := New("legacy", 9, 1, 1, NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	for index := range item.Nodes {
		item.Nodes[index].IP = ""
	}
	if err := Save(item); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := []string{loaded.Nodes[0].IP, loaded.Nodes[1].IP}, []string{"172.30.9.2", "172.30.9.3"}; !equalStrings(got, want) {
		t.Fatalf("legacy reservations = %v, want %v", got, want)
	}
	dir, err := Dir(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		t.Fatal(err)
	}
	var persisted Cluster
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if got, want := []string{persisted.Nodes[0].IP, persisted.Nodes[1].IP}, []string{"172.30.9.2", "172.30.9.3"}; !equalStrings(got, want) {
		t.Fatalf("persisted legacy reservations = %v, want %v", got, want)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
