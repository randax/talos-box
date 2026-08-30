package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A save remembers whether the guest had a balloon device, and the marker
// goes away with the save (#513).
func TestSaveStateBalloonMarker(t *testing.T) {
	save := filepath.Join(t.TempDir(), "cp-1"+saveStateSuffix)
	if err := os.WriteFile(save, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recordSaveStateBalloon(save, false); err != nil || savedWithoutBalloon(save) {
		t.Fatalf("balloon-enabled save: err=%v marked=%t", err, savedWithoutBalloon(save))
	}
	if err := recordSaveStateBalloon(save, true); err != nil || !savedWithoutBalloon(save) {
		t.Fatalf("balloon-less save: err=%v marked=%t", err, savedWithoutBalloon(save))
	}
	if err := recordSaveStateBalloon(save, false); err != nil || savedWithoutBalloon(save) {
		t.Fatalf("re-save with balloon: err=%v marked=%t", err, savedWithoutBalloon(save))
	}
	if err := recordSaveStateBalloon(save, true); err != nil {
		t.Fatal(err)
	}
	removeSaveStateFiles([]string{save})
	if savedWithoutBalloon(save) {
		t.Fatal("marker survived removeSaveStateFiles")
	}
	if got := saveStateBalloonPath("n" + saveStateSuffix); got != "n"+saveStateBalloonSuffix {
		t.Fatalf("saveStateBalloonPath = %q, want suffix %q", got, saveStateBalloonSuffix)
	}
}

// A marker that cannot be written or cleared fails the record, so the caller
// can refuse to report a save it would later misread (#513).
func TestRecordSaveStateBalloonReportsMarkerFailures(t *testing.T) {
	save := filepath.Join(t.TempDir(), "cp-1"+saveStateSuffix)
	// a non-empty directory at the marker path defeats both WriteFile and Remove
	if err := os.MkdirAll(filepath.Join(saveStateBalloonPath(save), "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := recordSaveStateBalloon(save, true); err == nil {
		t.Fatal("recordSaveStateBalloon(disabled) = nil with an unwritable marker")
	}
	if err := recordSaveStateBalloon(save, false); err == nil {
		t.Fatal("recordSaveStateBalloon(enabled) = nil with a stale marker that cannot be removed")
	}
}

// A marker that outlives its save is reported by every path that discards
// saves, so the operator learns about it before the next suspend fails (#513).
func TestStaleBalloonMarkerIsReportedByDiscardPaths(t *testing.T) {
	dir := t.TempDir()
	stuck := func(node string) string {
		save := saveStatePath(dir, node)
		if err := os.WriteFile(save, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		// a non-empty directory at the marker path cannot be removed
		if err := os.MkdirAll(filepath.Join(saveStateBalloonPath(save), "child"), 0o700); err != nil {
			t.Fatal(err)
		}
		return save
	}
	save := stuck("cp-1")
	warnings := removeSaveStateFiles([]string{save})
	if len(warnings) != 1 || !strings.Contains(warnings[0], "cp-1") || !strings.Contains(warnings[0], saveStateBalloonSuffix) {
		t.Fatalf("removeSaveStateFiles warnings = %v, want one naming cp-1 and the marker", warnings)
	}
	if _, err := os.Stat(save); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("save itself was not removed")
	}

	stuck("cp-2")
	discarded, warning := discardSavedState(dir, "cp-2")
	if !discarded || !strings.Contains(warning, "cp-2") {
		t.Fatalf("discardSavedState = %t, %q; want discarded with a cp-2 marker warning", discarded, warning)
	}

	stuck("cp-3")
	discarded, failures := discardClusterSavedStates(dir)
	if !discarded || len(failures) != 1 || !strings.Contains(failures[0], "cp-3") {
		t.Fatalf("discardClusterSavedStates = %t, %v; want discarded with one cp-3 marker warning", discarded, failures)
	}
}
