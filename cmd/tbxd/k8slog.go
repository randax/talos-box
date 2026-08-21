package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

// kubernetesLogFile holds everything the Kubernetes client libraries say.
// tbxd.log is the file the runbooks point operators at for tbx's own lifecycle
// narration, and klog's `I0819 13:37:07.217099` lines — PodSecurity violations
// and chart deprecation warnings among them — used to land between the balloon
// and node.remove lines an operator was trying to follow (#401).
const kubernetesLogFile = "tbxd.k8s.log"

// kubernetesWarningsEnv opts the client-go API warning handler back in. The
// warnings are upstream chart and workload findings, not tbx defects, so they
// are suppressed by default; set it to keep them (in the Kubernetes log).
const kubernetesWarningsEnv = "TBXD_K8S_WARNINGS"

// setDefaultWarningHandler is a var only so tests can observe the handler tbxd
// installs without reading client-go's package-level state.
var setDefaultWarningHandler = rest.SetDefaultWarningHandler

func kubernetesLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".talosbox", kubernetesLogFile), nil
}

// routeKubernetesClientLogs sends klog and client-go output to their own file
// and silences the API warning handler unless the operator asked for it. The
// returned closer flushes and closes that file.
func routeKubernetesClientLogs(path string, warningsEnabled bool) (io.Closer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	// LogToStderr(false) alone diverts only Info and Warning: klog keeps a
	// separate stderr threshold that defaults to ERROR, so every E-line would
	// still be written to os.Stderr — which for tbxd is tbxd.log — on top of
	// the file below, leaving #401's interleaving half fixed. one_output stops
	// klog from also copying each line down into every lower severity's writer,
	// which with a single writer means duplicates.
	var klogFlags flag.FlagSet
	klog.InitFlags(&klogFlags)
	if err := klogFlags.Set("stderrthreshold", "FATAL"); err != nil {
		return nil, fmt.Errorf("set klog stderr threshold: %w", err)
	}
	if err := klogFlags.Set("one_output", "true"); err != nil {
		return nil, fmt.Errorf("set klog one_output: %w", err)
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	klog.LogToStderr(false)
	klog.SetOutput(file)
	if warningsEnabled {
		setDefaultWarningHandler(rest.WarningLogger{})
	} else {
		setDefaultWarningHandler(rest.NoWarnings{})
	}
	return kubernetesLogCloser{file: file}, nil
}

type kubernetesLogCloser struct{ file *os.File }

func (c kubernetesLogCloser) Close() error {
	klog.Flush()
	return c.file.Close()
}

// startKubernetesLogRouting is the daemon's one-line wiring. Failing to route
// the client logs is never fatal: the daemon still serves, it just keeps the
// old interleaving, and says so.
func startKubernetesLogRouting(logf func(string, ...any)) io.Closer {
	path, err := kubernetesLogPath()
	if err == nil {
		var closer io.Closer
		closer, err = routeKubernetesClientLogs(path, os.Getenv(kubernetesWarningsEnv) != "")
		if err == nil {
			logf("kubernetes client logs go to %s", path)
			return closer
		}
	}
	logf("kubernetes client logs stay in this log: %v", err)
	return nil
}
