package hypervisor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestValidateQEMUSaveRejectsIdentityMismatch(t *testing.T) {
	currentVersion := "8.2.2"
	currentArchitecture := ArchitectureAMD64
	currentMachine := "q35"
	compatible := qemuSaveMetadata{
		Schema:       qemuSaveSchema,
		Backend:      qemuSaveBackend,
		QEMUVersion:  currentVersion,
		Architecture: currentArchitecture,
		Machine:      currentMachine,
	}

	tests := []struct {
		name     string
		metadata qemuSaveMetadata
		values   []string
	}{
		{
			name: "backend",
			metadata: func() qemuSaveMetadata {
				metadata := compatible
				metadata.Backend = "vz"
				return metadata
			}(),
			values: []string{"vz", compatible.Backend},
		},
		{
			name: "architecture",
			metadata: func() qemuSaveMetadata {
				metadata := compatible
				metadata.Architecture = ArchitectureARM64
				return metadata
			}(),
			values: []string{string(ArchitectureARM64), string(currentArchitecture)},
		},
		{
			name: "machine",
			metadata: func() qemuSaveMetadata {
				metadata := compatible
				metadata.Machine = "virt"
				return metadata
			}(),
			values: []string{"virt", currentMachine},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateQEMUSave(test.metadata, currentArchitecture, currentMachine)
			if !errors.Is(err, ErrIncompatibleSave) {
				t.Fatalf("validateQEMUSave() = %v, want ErrIncompatibleSave", err)
			}
			for _, value := range test.values {
				if !strings.Contains(err.Error(), value) {
					t.Fatalf("validateQEMUSave() = %q, want identifying value %q", err, value)
				}
			}
		})
	}
}

func TestValidateQEMUSaveVersionIsSeparateFromIdentity(t *testing.T) {
	metadata := qemuSaveMetadata{Schema: qemuSaveSchema, Backend: qemuSaveBackend, QEMUVersion: "8.2.1", Architecture: ArchitectureAMD64, Machine: "q35"}
	if err := validateQEMUSave(metadata, ArchitectureAMD64, "q35"); err != nil {
		t.Fatalf("validateQEMUSave() = %v, want a version-only difference to pass the identity check", err)
	}
	err := validateQEMUSaveVersion(metadata, "8.2.2")
	if !errors.Is(err, ErrIncompatibleSave) || !strings.Contains(err.Error(), "8.2.1") || !strings.Contains(err.Error(), "8.2.2") {
		t.Fatalf("validateQEMUSaveVersion() = %v, want ErrIncompatibleSave naming both versions", err)
	}
	if err := validateQEMUSaveVersion(metadata, "8.2.1"); err != nil {
		t.Fatalf("validateQEMUSaveVersion() = %v, want nil", err)
	}
}
