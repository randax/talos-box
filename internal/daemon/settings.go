package daemon

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// settingsFile holds the daemon-wide modes that must outlive the process. It
// sits beside the clusters under the talosbox root, so a mode an operator set
// is still in force after `tbx system restart` (#318).
const settingsFile = "daemon.json"

// settings is the on-disk shape of those modes. Every field must default to
// the daemon's normal behaviour, because a missing or unreadable file is read
// as the zero value rather than failing startup.
type settings struct {
	MirrorOffline bool `json:"mirrorOffline"`
}

// settingsPath names the daemon state file under the current user's home.
func settingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".talosbox", settingsFile), nil
}

// loadSettings reads the daemon state file. A missing file is not an error: it
// simply means no mode was ever set. A corrupt one returns the defaults along
// with the reason, so a caller can log it and keep serving.
func loadSettings(path string) (settings, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return settings{}, nil
	}
	if err != nil {
		return settings{}, fmt.Errorf("read daemon settings: %w", err)
	}
	var loaded settings
	if err := json.Unmarshal(data, &loaded); err != nil {
		return settings{}, fmt.Errorf("decode daemon settings %s: %w", path, err)
	}
	return loaded, nil
}

// saveSettings installs the daemon state file atomically, so a crash mid-write
// can never leave a half-written file that reads as a different mode.
func saveSettings(path string, current settings) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create daemon settings directory: %w", err)
	}
	data, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return fmt.Errorf("encode daemon settings: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, "."+settingsFile+"-*")
	if err != nil {
		return fmt.Errorf("create temporary daemon settings: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set daemon settings permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write daemon settings: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync daemon settings: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close daemon settings: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install daemon settings: %w", err)
	}
	return nil
}

// applyPersistedSettings re-applies the daemon-wide modes stored on disk and
// remembers where later changes are written. It never fails: an unusable
// settings file means the defaults, not a daemon that refuses to start (#318).
func (s *Server) applyPersistedSettings() {
	path, err := settingsPath()
	if err != nil {
		log.Printf("daemon settings: %v; continuing with defaults", err)
		return
	}
	s.settingsPath = path
	stored, err := loadSettings(path)
	if err != nil {
		log.Printf("daemon settings: %v; continuing with defaults", err)
	}
	s.mirrorOffline.Store(stored.MirrorOffline)
	if s.mirrors != nil {
		s.mirrors.SetOffline(stored.MirrorOffline)
	}
}

// updateSettings applies one change to the stored settings, preserving every
// known field it does not touch. The rewrite is from the parsed struct, so a
// key the current daemon does not know — one a newer daemon wrote — is dropped.
func updateSettings(path string, apply func(*settings)) error {
	current, err := loadSettings(path)
	if err != nil {
		// A corrupt file must not wedge the daemon: rewrite it from defaults.
		current = settings{}
	}
	apply(&current)
	return saveSettings(path, current)
}
