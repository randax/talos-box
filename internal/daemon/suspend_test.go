package daemon

import (
	"errors"
	"testing"
)

func TestResumeNodeRestoresWhenSaveValid(t *testing.T) {
	var restored, coldBooted bool
	warning, err := resumeNode(true,
		func() error { restored = true; return nil },
		func() error { coldBooted = true; return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !restored || coldBooted {
		t.Errorf("valid save: restored=%v coldBooted=%v, want restore only", restored, coldBooted)
	}
	if warning != "" {
		t.Errorf("valid restore should not warn, got %q", warning)
	}
}

func TestResumeNodeColdBootsWhenSaveMissing(t *testing.T) {
	var restored, coldBooted bool
	warning, err := resumeNode(false,
		func() error { restored = true; return nil },
		func() error { coldBooted = true; return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if restored || !coldBooted {
		t.Errorf("missing save: restored=%v coldBooted=%v, want cold boot only", restored, coldBooted)
	}
	if warning == "" {
		t.Error("missing save should produce a warning")
	}
}

func TestResumeNodeColdBootsWhenRestoreFails(t *testing.T) {
	var coldBooted bool
	warning, err := resumeNode(true,
		func() error { return errors.New("incompatible saved state") },
		func() error { coldBooted = true; return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !coldBooted {
		t.Error("failed restore must fall back to cold boot")
	}
	if warning == "" {
		t.Error("failed restore should produce a warning")
	}
}

func TestResumeNodePropagatesColdBootFailure(t *testing.T) {
	_, err := resumeNode(false,
		func() error { return nil },
		func() error { return errors.New("no image") },
	)
	if err == nil {
		t.Fatal("cold-boot failure must surface (nothing else to fall back to)")
	}
}
