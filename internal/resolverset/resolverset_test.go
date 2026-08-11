package resolverset

import (
	"reflect"
	"testing"
)

func TestPlanCreatesMissingAndDriftedFiles(t *testing.T) {
	observed := map[string][]byte{
		"lab.internal": []byte(Marker + "\nstale managed content\n"),
	}
	create, remove := Plan([]string{"lab.internal", "corp.example.com"}, observed, 5399)
	if want := []string{"corp.example.com", "lab.internal"}; !reflect.DeepEqual(create, want) {
		t.Errorf("create = %v, want %v", create, want)
	}
	if len(remove) != 0 {
		t.Errorf("remove = %v, want none", remove)
	}
}

func TestPlanNeverOverwritesUnmanagedWantedFile(t *testing.T) {
	// The user already has their own /etc/resolver/lab.internal; creating a
	// cluster with that domain must not clobber it as root.
	observed := map[string][]byte{
		"lab.internal": []byte("nameserver 10.0.0.1\n"),
	}
	create, remove := Plan([]string{"lab.internal"}, observed, 5399)
	if len(create) != 0 || len(remove) != 0 {
		t.Errorf("create = %v remove = %v, want none (unmanaged conflict)", create, remove)
	}
}

func TestPlanIsEmptyWhenConverged(t *testing.T) {
	observed := map[string][]byte{
		"lab.internal": []byte(Content(5399)),
	}
	create, remove := Plan([]string{"lab.internal"}, observed, 5399)
	if len(create) != 0 || len(remove) != 0 {
		t.Errorf("create = %v remove = %v, want none", create, remove)
	}
}

func TestPlanRemovesOnlyMarkedOrphans(t *testing.T) {
	observed := map[string][]byte{
		"gone.internal": []byte(Content(5399)),                       // ours, cluster deleted
		"k8s.test":      []byte("nameserver 127.0.0.1\nport 5399\n"), // shared default file, unmarked
		"corp.vpn":      []byte("nameserver 10.0.0.1\n"),             // the user's own file
	}
	create, remove := Plan(nil, observed, 5399)
	if len(create) != 0 {
		t.Errorf("create = %v, want none", create)
	}
	if want := []string{"gone.internal"}; !reflect.DeepEqual(remove, want) {
		t.Errorf("remove = %v, want %v", remove, want)
	}
}

func TestContentCarriesOwnershipMarker(t *testing.T) {
	content := Content(5399)
	if !Managed([]byte(content)) {
		t.Fatal("Content is not recognized as managed")
	}
	if Managed([]byte("nameserver 127.0.0.1\nport 5399\n")) {
		t.Fatal("unmarked content recognized as managed")
	}
	if Managed([]byte(Marker + " backup of my old resolver\nnameserver 10.0.0.1\n")) {
		t.Fatal("marker-prefixed user file recognized as managed")
	}
}
