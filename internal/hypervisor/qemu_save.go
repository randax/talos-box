package hypervisor

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// qemuSaveOffset leaves a page-aligned prefix for talos-box metadata. QEMU's
// file migration transport explicitly supports an offset for this purpose.
const qemuSaveOffset int64 = qemuIncomingOffset

const qemuSaveSchema = 1

type qemuSaveMetadata struct {
	Schema       int          `json:"schema"`
	Backend      string       `json:"backend"`
	QEMUVersion  string       `json:"qemu_version"`
	Architecture Architecture `json:"architecture"`
	Machine      string       `json:"machine"`
}

func prepareQEMUSave(path string, metadata qemuSaveMetadata) (string, error) {
	metadata.Schema = qemuSaveSchema
	metadata.Backend = "qemu"
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("encode QEMU save metadata: %w", err)
	}
	if int64(len(encoded)+1) >= qemuSaveOffset {
		return "", errors.New("QEMU save metadata exceeds reserved header")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create QEMU save directory: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return "", fmt.Errorf("create QEMU save: %w", err)
	}
	temporary := file.Name()
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(temporary)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return "", fmt.Errorf("protect QEMU save: %w", err)
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		return "", fmt.Errorf("write QEMU save metadata: %w", err)
	}
	if err := file.Truncate(qemuSaveOffset); err != nil {
		return "", fmt.Errorf("reserve QEMU save header: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close QEMU save header: %w", err)
	}
	keep = true
	return temporary, nil
}

func commitQEMUSave(temporary, destination string) error {
	file, err := os.Open(temporary)
	if err != nil {
		return fmt.Errorf("open completed QEMU save: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync completed QEMU save: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close completed QEMU save: %w", err)
	}
	if err := os.Rename(temporary, destination); err != nil {
		return fmt.Errorf("commit QEMU save: %w", err)
	}
	directory, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return fmt.Errorf("open QEMU save directory: %w", err)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("sync QEMU save directory: %w", err)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close QEMU save directory: %w", err)
	}
	return nil
}

func readQEMUSave(path string) (qemuSaveMetadata, error) {
	file, err := os.Open(path)
	if err != nil {
		return qemuSaveMetadata{}, fmt.Errorf("%w: open save: %v", ErrIncompatibleSave, err)
	}
	defer func() { _ = file.Close() }()

	line, err := bufio.NewReader(io.LimitReader(file, qemuSaveOffset)).ReadBytes('\n')
	if err != nil {
		return qemuSaveMetadata{}, fmt.Errorf("%w: read save metadata: %v", ErrIncompatibleSave, err)
	}
	var metadata qemuSaveMetadata
	if err := json.Unmarshal(line, &metadata); err != nil {
		return qemuSaveMetadata{}, fmt.Errorf("%w: decode save metadata: %v", ErrIncompatibleSave, err)
	}
	if metadata.Schema != qemuSaveSchema || metadata.Backend != "qemu" || metadata.QEMUVersion == "" || metadata.Machine == "" {
		return qemuSaveMetadata{}, fmt.Errorf("%w: unrecognized QEMU save metadata", ErrIncompatibleSave)
	}
	return metadata, nil
}

func validateQEMUSave(metadata qemuSaveMetadata, version string, architecture Architecture, machine string) error {
	switch {
	case metadata.QEMUVersion != version:
		return fmt.Errorf("%w: save uses QEMU %s, host has QEMU %s", ErrIncompatibleSave, metadata.QEMUVersion, version)
	case metadata.Architecture != architecture:
		return fmt.Errorf("%w: save targets %s, host targets %s", ErrIncompatibleSave, metadata.Architecture, architecture)
	case metadata.Machine != machine:
		return fmt.Errorf("%w: save uses machine %s, host requires %s", ErrIncompatibleSave, metadata.Machine, machine)
	default:
		return nil
	}
}
