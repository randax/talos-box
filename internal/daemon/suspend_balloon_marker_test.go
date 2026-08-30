package daemon

import (
	"os"
	"path/filepath"
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
