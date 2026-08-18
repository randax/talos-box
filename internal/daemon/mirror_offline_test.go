package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/randax/talos-box/internal/mirror"
)

// newPersistentServer builds a server whose daemon-wide modes are stored under
// an isolated HOME, which is what a restart re-reads.
func newPersistentServer(t *testing.T) *Server {
	t.Helper()
	service := &Server{mirrors: mirror.NewManager(t.TempDir())}
	service.applyPersistedSettings()
	return service
}

// TestMirrorOfflineSurvivesADaemonRestart pins the QA contract that the offline
// mode is a stored setting, not a process-lifetime flag (#318).
func TestMirrorOfflineSurvivesADaemonRestart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	first := newPersistentServer(t)
	if _, err := first.handle(Request{Op: "mirror.offline.set", Args: json.RawMessage(`{"enabled":true}`)}); err != nil {
		t.Fatal(err)
	}

	restarted := newPersistentServer(t)
	if !restarted.mirrorOffline.Load() {
		t.Fatal("mirror offline did not survive the restart")
	}
	if !restarted.mirrors.Offline() {
		t.Fatal("the restarted mirror manager did not observe the stored offline mode")
	}
	got, err := restarted.handle(Request{Op: "mirror.offline.get", Args: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if status, ok := got.(MirrorOfflineStatus); !ok || !status.Enabled {
		t.Fatalf("mirror.offline.get after restart = %#v, want enabled", got)
	}

	if _, err := restarted.handle(Request{Op: "mirror.offline.set", Args: json.RawMessage(`{"enabled":false}`)}); err != nil {
		t.Fatal(err)
	}
	if newPersistentServer(t).mirrorOffline.Load() {
		t.Fatal("turning the mode off did not survive the restart either")
	}
}

// TestMirrorOfflineToleratesACorruptSettingsFile keeps a broken state file from
// taking the daemon down with it: it means "off", and startup continues.
func TestMirrorOfflineToleratesACorruptSettingsFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".talosbox"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".talosbox", settingsFile)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	service := newPersistentServer(t)
	if service.mirrorOffline.Load() || service.mirrors.Offline() {
		t.Fatal("a corrupt settings file must read as offline mode off")
	}

	// The corrupt file must not block a later change from being stored.
	if _, err := service.handle(Request{Op: "mirror.offline.set", Args: json.RawMessage(`{"enabled":true}`)}); err != nil {
		t.Fatal(err)
	}
	if !newPersistentServer(t).mirrorOffline.Load() {
		t.Fatal("mirror offline was not stored over the corrupt settings file")
	}
}

// TestSettingsRoundTripStaysPrivate checks the stored file round-trips and is
// written 0600, like every other file under the talosbox root.
func TestSettingsRoundTripStaysPrivate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".talosbox", settingsFile)
	if err := saveSettings(path, settings{MirrorOffline: true}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("daemon settings permissions = %v, want 0600", perm)
	}
	stored, err := loadSettings(path)
	if err != nil || !stored.MirrorOffline {
		t.Fatalf("loadSettings = %#v, %v, want the stored mode", stored, err)
	}
}

func TestMirrorOfflineDefaultsOffAndCanToggle(t *testing.T) {
	t.Parallel()

	service := &Server{mirrors: mirror.NewManager(t.TempDir())}

	got, err := service.handle(Request{Op: "mirror.offline.get", Args: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	initial, ok := got.(MirrorOfflineStatus)
	if !ok {
		t.Fatalf("get result type = %T, want MirrorOfflineStatus", got)
	}
	if initial.Enabled {
		t.Fatal("mirror offline default = on, want off")
	}

	got, err = service.handle(Request{Op: "mirror.offline.set", Args: json.RawMessage(`{"enabled":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	enabled, ok := got.(MirrorOfflineStatus)
	if !ok {
		t.Fatalf("set result type = %T, want MirrorOfflineStatus", got)
	}
	if !enabled.Enabled {
		t.Fatal("mirror offline set did not enable state")
	}
	if !service.mirrors.Offline() {
		t.Fatal("mirror manager did not observe enabled state")
	}

	got, err = service.handle(Request{Op: "mirror.offline.get", Args: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	after, ok := got.(MirrorOfflineStatus)
	if !ok {
		t.Fatalf("get-after-set result type = %T, want MirrorOfflineStatus", got)
	}
	if !after.Enabled {
		t.Fatal("mirror offline inspection did not report enabled state")
	}
}

func TestMirrorOfflineSetRejectsMissingNullAndUnknownPayloads(t *testing.T) {
	t.Parallel()

	service := &Server{mirrors: mirror.NewManager(t.TempDir())}
	for _, payload := range []string{
		`{}`,
		`{"enabled":null}`,
		`{"enabled":true,"extra":1}`,
		`{"enabled":true}{"enabled":false}`,
		`{"enabled":true,"enabled":false}`,
		`{"extra":1}`,
		`true`,
	} {
		t.Run(payload, func(t *testing.T) {
			_, err := service.handle(Request{Op: "mirror.offline.set", Args: json.RawMessage(payload)})
			if err == nil {
				t.Fatalf("mirror.offline.set accepted invalid payload %s", payload)
			}
		})
	}
}
