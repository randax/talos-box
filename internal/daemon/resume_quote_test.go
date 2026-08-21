package daemon

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// vzInvalidArgumentCause is the exact cause the QA battery hit in #412: the
// reason worth reading — “invalid argument” — sits just past the old fixed
// budget, so a blind cut rendered it as `“invalid...”`.
const vzInvalidArgumentCause = `hypervisor saved state is incompatible: Error Domain=VZErrorDomain Code=12 Description="An error occurred while restoring the virtual machine. The virtual machine failed to restore with error “invalid argument”."`

// TestPlatformErrorSummaryKeepsAShortQuotedReasonWhole pins #412: #361 closed
// the dangling quote but still cut inside it, so the operator read
// `“invalid...”` for a 16-character reason. A quoted reason that ends within
// the slack must survive whole.
func TestPlatformErrorSummaryKeepsAShortQuotedReasonWhole(t *testing.T) {
	summary := platformErrorSummary(errors.New(vzInvalidArgumentCause))
	if !strings.Contains(summary, `“invalid argument”`) {
		t.Fatalf("summary = %q, want the quoted reason whole", summary)
	}
	if strings.Contains(summary, "...") {
		t.Fatalf("summary = %q, want no elision at all for a cause this short", summary)
	}
	warning := coldBootWarning("qa-sta-cp-1", false, errors.New(vzInvalidArgumentCause))
	if !strings.Contains(warning, `“invalid argument”`) {
		t.Fatalf("warning = %q, want the quoted reason whole", warning)
	}
}

// TestPlatformErrorSummaryNeverTruncatesInsideQuotes covers the general rule:
// wherever the budget lands, the elision marker is never inside a quoted span.
func TestPlatformErrorSummaryNeverTruncatesInsideQuotes(t *testing.T) {
	filler := strings.Repeat("x", maxPlatformErrorSummary)
	tests := []struct {
		name      string
		cause     string
		wantParts []string
	}{
		{
			name:      "quote closing within the slack is carried whole",
			cause:     `failed with ` + strings.Repeat("y", maxPlatformErrorSummary-17) + `“invalid argument” and then ` + filler,
			wantParts: []string{`“invalid argument”`, `...`},
		},
		{
			name:      "quote longer than the slack is cut before it opens",
			cause:     `failed with “` + strings.Repeat("z", 4096) + `”`,
			wantParts: []string{`failed with...`},
		},
		{
			name:      "unterminated quote is cut before it opens",
			cause:     `failed with “` + strings.Repeat("z", maxPlatformErrorSummary),
			wantParts: []string{`failed with...`},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			summary := platformErrorSummary(errors.New(test.cause))
			for _, want := range test.wantParts {
				if !strings.Contains(summary, want) {
					t.Fatalf("summary = %q, want substring %q", summary, want)
				}
			}
			if index := strings.Index(summary, "..."); index >= 0 {
				if _, open := unclosedQuoteStart([]rune(summary[:index])); open {
					t.Fatalf("summary = %q truncates inside a quoted span", summary)
				}
			}
			if got := strings.Count(summary, "“") - strings.Count(summary, "”"); got != 0 {
				t.Fatalf("summary = %q, leaves %d curly quotes open", summary, got)
			}
			if got := strings.Count(summary, `"`); got%2 != 0 {
				t.Fatalf("summary = %q, leaves a straight quote open", summary)
			}
			if !utf8.ValidString(summary) {
				t.Fatalf("summary = %q, want valid UTF-8", summary)
			}
		})
	}
}

// TestPlatformErrorSummaryDropsAQuoteOpeningAtTheStart covers the case neither
// truncation branch used to fire on: a quote that opens at index 0 and does not
// close within the slack left the cut at the fixed budget, landing mid-quote —
// the exact #412 defect. With nothing before the quote to keep, the summary is
// dropped and the warning falls back to its log pointer.
func TestPlatformErrorSummaryDropsAQuoteOpeningAtTheStart(t *testing.T) {
	for _, cause := range []string{
		`“` + strings.Repeat("z", 4096) + `”`,
		`"` + strings.Repeat("z", 4096),
	} {
		if got := platformErrorSummary(errors.New(cause)); got != "" {
			t.Fatalf("summary = %q, want it dropped rather than cut inside the quote", got)
		}
	}
	warning := coldBootWarning("qa-sta-cp-1", false, errors.New(`“`+strings.Repeat("z", 4096)+`”`))
	if strings.Contains(warning, "z") || !strings.HasSuffix(warning, "(details: ~/.talosbox/tbxd.log)") {
		t.Fatalf("warning = %q, want the bare cold-boot warning with the log pointer", warning)
	}
}
