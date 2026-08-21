package provision

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// A gate that keeps failing must say what it is failing on: a provisioning pass
// that stalls here is otherwise completely silent for its whole budget (#390).
func TestPollReportsItsBlockerToTheGateObserver(t *testing.T) {
	var gates []Gate
	var blockers []string
	ctx, cancel := context.WithCancel(context.Background())
	observed := 0
	ctx = WithGateObserver(ctx, func(gate Gate, blocker error) {
		gates = append(gates, gate)
		blockers = append(blockers, blocker.Error())
		if observed++; observed == 3 {
			cancel()
		}
	})
	defer cancel()

	err := poll(ctx, GateLonghorn, time.Millisecond, func(context.Context) error {
		return errors.New("longhorn manager is not Ready")
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("poll() error = %v, want cancellation", err)
	}
	if len(gates) < 3 {
		t.Fatalf("gate reports = %d, want the gate to keep naming itself", len(gates))
	}
	for i, gate := range gates {
		if gate != GateLonghorn {
			t.Fatalf("gate report %d = %q, want %q", i, gate, GateLonghorn)
		}
		if blockers[i] != "longhorn manager is not Ready" {
			t.Fatalf("blocker %d = %q, want the check's own observation", i, blockers[i])
		}
	}
}

// A terminal failure is the gate's answer, not something it is waiting on, so
// it is never reported as a blocker — and a pass with no observer installed
// must poll exactly as it always did.
func TestPollDoesNotReportTerminalFailuresOrRequireAnObserver(t *testing.T) {
	reported := 0
	ctx := WithGateObserver(context.Background(), func(Gate, error) { reported++ })
	if err := poll(ctx, GateStorageProbePVC, time.Millisecond, func(context.Context) error {
		return terminal(errors.New("wrong StorageClass"))
	}); err == nil || err.Error() != "wrong StorageClass" {
		t.Fatalf("poll() error = %v, want the terminal failure", err)
	}
	if reported != 0 {
		t.Fatalf("gate reports = %d, want none for a terminal failure", reported)
	}
	if err := poll(context.Background(), GateStorageProbePVC, time.Millisecond, func(context.Context) error {
		return nil
	}); err != nil {
		t.Fatalf("poll() without an observer error = %v", err)
	}
}

func TestBlockerMessageFoldsAndCaps(t *testing.T) {
	for _, tt := range []struct {
		name    string
		blocker error
		want    string
	}{
		{name: "nil", blocker: nil, want: ""},
		{
			name:    "joined observations fold onto one line",
			blocker: errors.Join(errors.New("PVC pending"), errors.New("pod Pending")),
			want:    "PVC pending; pod Pending",
		},
		{name: "single line survives", blocker: errors.New("not Ready"), want: "not Ready"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := BlockerMessage(tt.blocker); got != tt.want {
				t.Fatalf("BlockerMessage() = %q, want %q", got, tt.want)
			}
		})
	}
	long := BlockerMessage(errors.New(strings.Repeat("x", 400)))
	if len([]rune(long)) != 201 || !strings.HasSuffix(long, "…") {
		t.Fatalf("BlockerMessage() length = %d, want a capped, elided line", len([]rune(long)))
	}
	// A multi-byte rune straddling the cut must not be sliced in half: the
	// message ends up in tbxd.log and in the storageError JSON field.
	multibyte := BlockerMessage(errors.New(strings.Repeat("a", 198) + "…tail"))
	if !utf8.ValidString(multibyte) {
		t.Fatalf("BlockerMessage() = %q, want valid UTF-8", multibyte)
	}
	if len([]rune(multibyte)) != 201 || !strings.HasSuffix(multibyte, "…") {
		t.Fatalf("BlockerMessage() length = %d, want a capped, elided line", len([]rune(multibyte)))
	}
}
