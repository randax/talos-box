package hypervisor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestQEMUSaveEnvelopeRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.vzstate")
	want := qemuSaveMetadata{
		QEMUVersion:  "8.2.2",
		Architecture: ArchitectureAMD64,
		Machine:      "q35",
	}
	temporary, err := prepareQEMUSave(path, want)
	if err != nil {
		t.Fatal(err)
	}
	if temporary == path {
		t.Fatal("save was written directly instead of through an atomic temporary file")
	}
	info, err := os.Stat(temporary)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != qemuSaveOffset {
		t.Fatalf("header size = %d, want %d", info.Size(), qemuSaveOffset)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("save permissions = %o, want 600", info.Mode().Perm())
	}
	if err := commitQEMUSave(temporary, path); err != nil {
		t.Fatal(err)
	}
	got, err := readQEMUSave(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.QEMUVersion != want.QEMUVersion || got.Architecture != want.Architecture || got.Machine != want.Machine {
		t.Fatalf("metadata = %#v, want %#v", got, want)
	}
}

func TestReadQEMUSaveRejectsMissingOrCorruptMetadata(t *testing.T) {
	for name, path := range map[string]string{
		"missing": filepath.Join(t.TempDir(), "missing"),
		"corrupt": func() string {
			path := filepath.Join(t.TempDir(), "corrupt")
			if err := os.WriteFile(path, []byte("not-json\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := readQEMUSave(path)
			if !errors.Is(err, ErrIncompatibleSave) {
				t.Fatalf("readQEMUSave() = %v, want ErrIncompatibleSave", err)
			}
		})
	}
}

func TestValidateQEMUSaveRejectsCrossVersionRestore(t *testing.T) {
	metadata := qemuSaveMetadata{
		Schema:       qemuSaveSchema,
		Backend:      "qemu",
		QEMUVersion:  "8.2.1",
		Architecture: ArchitectureAMD64,
		Machine:      "q35",
	}
	if err := validateQEMUSave(metadata, "8.2.2", ArchitectureAMD64, "q35"); !errors.Is(err, ErrIncompatibleSave) {
		t.Fatalf("validateQEMUSave() = %v, want ErrIncompatibleSave", err)
	}
}
