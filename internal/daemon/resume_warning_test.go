package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
)

// TestResumeColdBootWarningCarriesTheHypervisorCause pins #291: the daemon
// already logs why the restore failed, so the CLI-facing warning must say it
// too instead of leaving the operator with an unexplained cold boot.
func TestResumeColdBootWarningCarriesTheHypervisorCause(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("fake-cause", 0, 1, 0, cluster.NodeDefaults{CPUs: 1, MemoryMiB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	savePath := filepath.Join(dir, item.Nodes[0].Name+".vzstate")
	if err := os.WriteFile(savePath, []byte("saved"), 0o600); err != nil {
		t.Fatal(err)
	}

	cause := fmt.Errorf("%w: Error Domain=VZErrorDomain Code=12", hypervisor.ErrIncompatibleSave)
	backend := &fakeHypervisor{launch: func(_ context.Context, spec hypervisor.Spec) (hypervisor.Machine, error) {
		spec.Restore.Fallback(cause)
		return &fakeMachine{active: true}, nil
	}}
	service := &Server{
		hypervisors:   singleFakeRegistry(backend),
		vms:           make(map[string]map[string]hypervisor.Machine),
		subnetSources: emptySubnetSources(),
	}

	result, err := service.resumeCluster([]byte(`{"name":"fake-cause"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		item.Nodes[0].Name,
		"saved state could not be restored; cold-booting instead",
		"VZErrorDomain Code=12",
	} {
		if !strings.Contains(result.Warning, want) {
			t.Fatalf("resume warning = %q, want substring %q", result.Warning, want)
		}
	}
	if len(result.Warnings) != 1 || result.Warnings[0] != result.Warning {
		t.Fatalf("resume warnings = %q, want the single warning as its own entry", result.Warnings)
	}
}

// TestResumeMissingSaveWarningPointsAtTheDaemonLog keeps the missing-save case
// distinct and still actionable when there is no hypervisor cause to quote.
func TestResumeMissingSaveWarningPointsAtTheDaemonLog(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("fake-missing", 0, 1, 0, cluster.NodeDefaults{CPUs: 1, MemoryMiB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}

	backend := &fakeHypervisor{launch: func(_ context.Context, spec hypervisor.Spec) (hypervisor.Machine, error) {
		spec.Restore.Fallback(os.ErrNotExist)
		return &fakeMachine{active: true}, nil
	}}
	service := &Server{
		hypervisors:   singleFakeRegistry(backend),
		vms:           make(map[string]map[string]hypervisor.Machine),
		subnetSources: emptySubnetSources(),
	}

	result, err := service.resumeCluster([]byte(`{"name":"fake-missing"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Warning, "no saved state found; cold-booting instead") {
		t.Fatalf("resume warning = %q, want the missing-save wording", result.Warning)
	}
}

// TestResumeWarningsStayOnePerEntry pins #291's secondary: unrelated warnings
// must not be fused onto one line for the CLI.
func TestResumeWarningsStayOnePerEntry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("fake-multi", 0, 2, 0, cluster.NodeDefaults{CPUs: 1, MemoryMiB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range item.Nodes {
		if err := os.WriteFile(filepath.Join(dir, node.Name+".vzstate"), []byte("saved"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	backend := &fakeHypervisor{launch: func(_ context.Context, spec hypervisor.Spec) (hypervisor.Machine, error) {
		spec.Restore.Fallback(hypervisor.ErrIncompatibleSave)
		return &fakeMachine{active: true}, nil
	}}
	service := &Server{
		hypervisors:   singleFakeRegistry(backend),
		vms:           make(map[string]map[string]hypervisor.Machine),
		subnetSources: emptySubnetSources(),
	}

	result, err := service.resumeCluster([]byte(`{"name":"fake-multi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Warnings) != len(item.Nodes) {
		t.Fatalf("resume warnings = %q, want one per node", result.Warnings)
	}
	for _, warning := range result.Warnings {
		if strings.Contains(warning, "; cold-booting instead; ") {
			t.Fatalf("warning %q fuses two warnings onto one entry", warning)
		}
	}
}

// TestPlatformErrorSummaryKeepsTheTerminalWarningOnOneLine pins #312: Cocoa
// hands back a multi-line plist, and three suspended nodes meant three dumps
// scrolling the warnings away. Only the description belongs in the terminal.
func TestPlatformErrorSummaryKeepsTheTerminalWarningOnOneLine(t *testing.T) {
	cocoa := "Error Domain=VZErrorDomain Code=12 \"The virtual machine failed to restore from the saved state.\" " +
		"UserInfo={NSLocalizedFailure=Internal Virtualization error.,\n" +
		"    NSUnderlyingError=0x600002a1c0f0 {Error Domain=VZErrorDomain Code=12 UserInfo={\n" +
		"        NSLocalizedFailureReason = \"incompatible save\";\n" +
		"    }}}"
	cause := fmt.Errorf("%w: %s", hypervisor.ErrIncompatibleSave, cocoa)

	summary := platformErrorSummary(cause)
	if strings.ContainsAny(summary, "\n\r") {
		t.Fatalf("summary = %q, want a single line", summary)
	}
	if strings.Contains(summary, "UserInfo=") {
		t.Fatalf("summary = %q, want the plist dump dropped", summary)
	}
	for _, want := range []string{
		hypervisor.ErrIncompatibleSave.Error(),
		"VZErrorDomain Code=12",
		"The virtual machine failed to restore from the saved state.",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary = %q, want substring %q", summary, want)
		}
	}

	warning := coldBootWarning("qa-sta-cp-1", false, cause)
	if strings.ContainsAny(warning, "\n\r") {
		t.Fatalf("warning = %q, want a single line", warning)
	}
	if !strings.HasPrefix(warning, "qa-sta-cp-1: saved state could not be restored; cold-booting instead: ") {
		t.Fatalf("warning = %q, want the lead clause preserved", warning)
	}
	if !strings.HasSuffix(warning, "(details: ~/.talosbox/tbxd.log)") {
		t.Fatalf("warning = %q, want the log pointer kept", warning)
	}
}

// TestPlatformErrorSummaryBoundsARunawayCause covers a cause that is one very
// long line: the terminal gets a bounded form, the daemon log keeps the rest.
func TestPlatformErrorSummaryBoundsARunawayCause(t *testing.T) {
	cause := fmt.Errorf("%w: %s", hypervisor.ErrIncompatibleSave, strings.Repeat("x", 4096))
	summary := platformErrorSummary(cause)
	if len([]rune(summary)) > maxPlatformErrorSummary+len("...") {
		t.Fatalf("summary is %d runes, want at most %d", len([]rune(summary)), maxPlatformErrorSummary+3)
	}
	if !strings.HasSuffix(summary, "...") {
		t.Fatalf("summary = %q, want an elision marker", summary)
	}
}

// TestPlatformErrorSummaryBalancesTruncatedQuotes pins #361: the rune-count cut
// is blind to the platform's own quoting, so a summary must never hand the
// operator a dangling opening quote. Since #412 a quote that closes within the
// slack is carried whole, so the elision follows the closer instead of
// preceding it.
func TestPlatformErrorSummaryBalancesTruncatedQuotes(t *testing.T) {
	filler := strings.Repeat("x", maxPlatformErrorSummary)
	tests := []struct {
		name      string
		cause     string
		want      string   // exact summary, when short enough to spell out
		wantParts []string // substrings a truncated summary must carry
	}{
		{
			name:  "no truncation keeps the quotes untouched",
			cause: `failed with “invalid save state” from the hypervisor`,
			want:  `failed with “invalid save state” from the hypervisor`,
		},
		{
			name:      "a curly quote closing within the slack is carried whole",
			cause:     `failed with “invalid ` + filler + `” trailing`,
			wantParts: []string{`“invalid `, `”...`},
		},
		{
			name:      "a straight quote closing within the slack is carried whole",
			cause:     `failed with "invalid ` + filler + `" trailing`,
			wantParts: []string{`"invalid `, `"...`},
		},
		{
			name:      "truncation after balanced quotes adds nothing",
			cause:     `failed with “invalid save state” ` + filler,
			wantParts: []string{`“invalid save state”`, `x...`},
		},
		{
			name:      "truncation landing on the opening quote drops it",
			cause:     strings.Repeat("y", maxPlatformErrorSummary-1) + ` “invalid save state” tail`,
			wantParts: []string{"y..."},
		},
		{
			name:      "multibyte runes near the boundary stay intact",
			cause:     strings.Repeat("é", maxPlatformErrorSummary-3) + `“så” ` + filler,
			wantParts: []string{"é", `“så”...`},
		},
		{
			name:      "multibyte quote closed just inside the boundary",
			cause:     strings.Repeat("é", maxPlatformErrorSummary-4) + `“så” ` + filler,
			wantParts: []string{"é", `“så”...`},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			summary := platformErrorSummary(errors.New(test.cause))
			if test.want != "" && summary != test.want {
				t.Fatalf("summary = %q, want %q", summary, test.want)
			}
			for _, want := range test.wantParts {
				if !strings.Contains(summary, want) {
					t.Fatalf("summary = %q, want substring %q", summary, want)
				}
			}
			if !utf8.ValidString(summary) {
				t.Fatalf("summary = %q, want valid UTF-8", summary)
			}
			if got := strings.Count(summary, "“") - strings.Count(summary, "”"); got != 0 {
				t.Fatalf("summary = %q, leaves %d curly quotes open", summary, got)
			}
			if got := strings.Count(summary, `"`); got%2 != 0 {
				t.Fatalf("summary = %q, leaves a straight quote open", summary)
			}
		})
	}
}

// TestPlatformErrorSummaryHandlesAnEmptyCause keeps the no-cause warning on the
// log-pointer-only wording rather than emitting a dangling colon.
func TestPlatformErrorSummaryHandlesAnEmptyCause(t *testing.T) {
	if got := platformErrorSummary(nil); got != "" {
		t.Fatalf("platformErrorSummary(nil) = %q, want empty", got)
	}
	warning := coldBootWarning("qa-sta-cp-1", false, errors.New("\n"))
	if warning != "qa-sta-cp-1: saved state could not be restored; cold-booting instead (details: ~/.talosbox/tbxd.log)" {
		t.Fatalf("warning = %q, want the causeless wording", warning)
	}
}
