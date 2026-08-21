package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

// TestRouteKubernetesClientLogsKeepsKlogOutOfTheDaemonLog pins #401: klog's
// output must land in its own file, not between the tbx lifecycle lines the
// runbooks tell operators to follow in tbxd.log.
func TestRouteKubernetesClientLogsKeepsKlogOutOfTheDaemonLog(t *testing.T) {
	var daemonLog bytes.Buffer
	restoreLog := swapDefaultLogger(t, &daemonLog)
	defer restoreLog()

	path := filepath.Join(t.TempDir(), ".talosbox", kubernetesLogFile)
	closer, err := routeKubernetesClientLogs(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()

	log.Print("balloon qa-cil/qa-cil-cp-1: target=2048MiB")
	klog.Info("Warning: spec.template.metadata.annotations deprecated since v1.30")
	klog.Flush()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "deprecated since v1.30") {
		t.Fatalf("%s = %q, want the klog line", kubernetesLogFile, content)
	}
	if got := daemonLog.String(); strings.Contains(got, "deprecated since v1.30") {
		t.Fatalf("daemon log = %q, want no klog line", got)
	}
	if got := daemonLog.String(); !strings.Contains(got, "balloon qa-cil/qa-cil-cp-1") {
		t.Fatalf("daemon log = %q, want the tbx narration line", got)
	}
}

// The API warning handler is what emitted the PodSecurity blocks, so it is off
// unless the operator opts in — and even then it writes to the Kubernetes log.
func TestRouteKubernetesClientLogsInstallsTheWarningHandler(t *testing.T) {
	tests := []struct {
		name     string
		enabled  bool
		wantType rest.WarningHandler
	}{
		{name: "suppressed by default", enabled: false, wantType: rest.NoWarnings{}},
		{name: "opted back in", enabled: true, wantType: rest.WarningLogger{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var installed rest.WarningHandler
			original := setDefaultWarningHandler
			setDefaultWarningHandler = func(handler rest.WarningHandler) { installed = handler }
			defer func() { setDefaultWarningHandler = original }()

			closer, err := routeKubernetesClientLogs(filepath.Join(t.TempDir(), kubernetesLogFile), test.enabled)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = closer.Close() }()

			if fmt.Sprintf("%T", installed) != fmt.Sprintf("%T", test.wantType) {
				t.Fatalf("warning handler = %T, want %T", installed, test.wantType)
			}
		})
	}
}

// A routing failure must not take the daemon down: it reports and keeps the old
// interleaving.
func TestStartKubernetesLogRoutingSurvivesAFailure(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".talosbox"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	var reported []string
	closer := startKubernetesLogRouting(func(format string, args ...any) {
		reported = append(reported, fmt.Sprintf(format, args...))
	})
	if closer != nil {
		t.Fatalf("closer = %v, want none when routing failed", closer)
	}
	if len(reported) != 1 || !strings.Contains(reported[0], "stay in this log") {
		t.Fatalf("reported = %v, want the fallback explanation", reported)
	}
}

// swapDefaultLogger points the standard logger — which in tbxd is tbxd.log — at
// a buffer for the duration of a test.
func swapDefaultLogger(t *testing.T, buffer *bytes.Buffer) func() {
	t.Helper()
	original := log.Writer()
	log.SetOutput(buffer)
	return func() { log.SetOutput(original) }
}
