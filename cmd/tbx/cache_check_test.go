package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/provision"
)

func TestRunCacheWarmCheckRequestsOfflineVerification(t *testing.T) {
	t.Setenv("HOME", shortTestHome(t))
	socketPath, err := daemon.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	listPath := filepath.Join(t.TempDir(), "images.txt")
	ref := "docker.io/library/pause:3.10"
	if err := os.WriteFile(listPath, []byte(ref+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A plain check verifies the list plus the bootstrap-required set (#404).
	wantRefs := append([]string{ref}, provision.BootstrapRequiredImages()...)
	done := make(chan struct{})
	go serveDaemonRequests(t, listener, len(wantRefs), func(index int, request daemon.Request) daemon.Response {
		if request.Op != "cache.check" {
			t.Fatalf("request op = %q, want cache.check", request.Op)
		}
		var args daemon.CacheCheckArgs
		if err := json.Unmarshal(request.Args, &args); err != nil {
			t.Fatal(err)
		}
		if args.Deep {
			t.Fatal("cache.check deep = true, want false")
		}
		if len(args.Refs) != 1 || args.Refs[0] != wantRefs[index] {
			t.Fatalf("refs = %v, want [%q]", args.Refs, wantRefs[index])
		}
		return daemon.Response{OK: true, Data: mustJSON(t, daemon.CacheCheckResult{
			Entries:  []daemon.CacheCheckEntry{{Ref: wantRefs[index], Status: daemon.CacheCheckStatusComplete}},
			Complete: 1,
		})}
	}, done)

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	if err := command.run([]string{"cache", "warm", "--check", listPath}); err != nil {
		t.Fatal(err)
	}
	<-done

	wantStdout := "" +
		"\u2713 docker.io/library/pause:3.10 complete\n" +
		"\u2713 " + provision.KubernetesSandboxImage + " complete\n" +
		"summary: 2 complete, 0 failed\n"
	if got := stdout.String(); got != wantStdout {
		t.Fatalf("stdout = %q, want %q", got, wantStdout)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunCacheWarmCheckDeepRequestsDeepVerification(t *testing.T) {
	t.Setenv("HOME", shortTestHome(t))
	socketPath, err := daemon.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	listPath := filepath.Join(t.TempDir(), "images.txt")
	ref := "docker.io/library/pause:3.10"
	if err := os.WriteFile(listPath, []byte(ref+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A deep check verifies the list plus the bootstrap-required set.
	wantRefs := append([]string{ref}, provision.BootstrapRequiredImages()...)
	done := make(chan struct{})
	go serveDaemonRequests(t, listener, len(wantRefs), func(index int, request daemon.Request) daemon.Response {
		var args daemon.CacheCheckArgs
		if err := json.Unmarshal(request.Args, &args); err != nil {
			t.Fatal(err)
		}
		if !args.Deep {
			t.Fatal("cache.check deep = false, want true")
		}
		if len(args.Refs) != 1 || args.Refs[0] != wantRefs[index] {
			t.Fatalf("refs = %v, want [%q]", args.Refs, wantRefs[index])
		}
		if index == 0 {
			return daemon.Response{OK: true, Data: mustJSON(t, daemon.CacheCheckResult{
				Entries: []daemon.CacheCheckEntry{{Ref: wantRefs[index], Status: daemon.CacheCheckStatusFailed, Reason: "sha256:deadbeef blob corrupted"}},
				Failed:  1,
			})}
		}
		return daemon.Response{OK: true, Data: mustJSON(t, daemon.CacheCheckResult{
			Entries:  []daemon.CacheCheckEntry{{Ref: wantRefs[index], Status: daemon.CacheCheckStatusComplete}},
			Complete: 1,
		})}
	}, done)

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	err = command.run([]string{"cache", "warm", "--check", "--deep", listPath})
	<-done
	if err == nil || err.Error() != "cache check failed for 1 ref(s)" {
		t.Fatalf("err = %v, want cache check failed for 1 ref(s)", err)
	}
	wantStdout := "" +
		"\u2717 docker.io/library/pause:3.10 sha256:deadbeef blob corrupted\n" +
		"\u2713 " + provision.KubernetesSandboxImage + " complete\n" +
		"summary: 1 complete, 1 failed\n"
	if got := stdout.String(); got != wantStdout {
		t.Fatalf("stdout = %q, want %q", got, wantStdout)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

// TestRunCacheWarmCheckDeepFlagsMissingSandboxImage is the venue gate: a deep
// check has to fail when the CRI pod sandbox image is absent, even though no
// warm list names it, so the gap is found before anyone goes offline.
func TestRunCacheWarmCheckDeepFlagsMissingSandboxImage(t *testing.T) {
	t.Setenv("HOME", shortTestHome(t))
	socketPath, err := daemon.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	listPath := filepath.Join(t.TempDir(), "images.txt")
	ref := "registry.k8s.io/kube-apiserver:v1.36.2"
	if err := os.WriteFile(listPath, []byte(ref+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go serveDaemonRequests(t, listener, 2, func(index int, request daemon.Request) daemon.Response {
		var args daemon.CacheCheckArgs
		if err := json.Unmarshal(request.Args, &args); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			return daemon.Response{OK: true, Data: mustJSON(t, daemon.CacheCheckResult{
				Entries:  []daemon.CacheCheckEntry{{Ref: ref, Status: daemon.CacheCheckStatusComplete}},
				Complete: 1,
			})}
		}
		if len(args.Refs) != 1 || args.Refs[0] != provision.KubernetesSandboxImage {
			t.Fatalf("refs = %v, want [%q]", args.Refs, provision.KubernetesSandboxImage)
		}
		return daemon.Response{OK: true, Data: mustJSON(t, daemon.CacheCheckResult{
			Entries: []daemon.CacheCheckEntry{{
				Ref: provision.KubernetesSandboxImage, Status: daemon.CacheCheckStatusFailed, Reason: "manifest not cached",
			}},
			Failed: 1,
		})}
	}, done)

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	err = command.run([]string{"cache", "warm", "--check", "--deep", listPath})
	<-done
	if err == nil || err.Error() != "cache check failed for 1 ref(s)" {
		t.Fatalf("err = %v, want cache check failed for 1 ref(s)", err)
	}
	if !strings.Contains(stdout.String(), "\u2717 "+provision.KubernetesSandboxImage+" manifest not cached") {
		t.Fatalf("stdout does not flag the missing sandbox image: %q", stdout.String())
	}
}

// TestRunCacheWarmCheckDeepDoesNotDuplicateSandboxImage keeps the addition
// invisible for a list that already carries it \u2014 the derived sets do, now that
// cache pull warms it.
func TestRunCacheWarmCheckDeepDoesNotDuplicateSandboxImage(t *testing.T) {
	t.Setenv("HOME", shortTestHome(t))
	socketPath, err := daemon.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	listPath := filepath.Join(t.TempDir(), "images.txt")
	if err := os.WriteFile(listPath, []byte(provision.KubernetesSandboxImage+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go serveDaemonRequests(t, listener, 1, func(_ int, request daemon.Request) daemon.Response {
		var args daemon.CacheCheckArgs
		if err := json.Unmarshal(request.Args, &args); err != nil {
			t.Fatal(err)
		}
		if len(args.Refs) != 1 || args.Refs[0] != provision.KubernetesSandboxImage {
			t.Fatalf("refs = %v, want [%q]", args.Refs, provision.KubernetesSandboxImage)
		}
		return daemon.Response{OK: true, Data: mustJSON(t, daemon.CacheCheckResult{
			Entries:  []daemon.CacheCheckEntry{{Ref: provision.KubernetesSandboxImage, Status: daemon.CacheCheckStatusComplete}},
			Complete: 1,
		})}
	}, done)

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	if err := command.run([]string{"cache", "warm", "--check", "--deep", listPath}); err != nil {
		t.Fatalf("deep check err = %v, want nil", err)
	}
	<-done
	want := "\u2713 " + provision.KubernetesSandboxImage + " complete\nsummary: 1 complete, 0 failed\n"
	if got := stdout.String(); got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

// TestRunCacheWarmCheckShallowAlsoCoversTheSandboxImage: both check modes are
// offline-readiness gates, so a plain --check must not hand out an all-clear
// that a deep check would fail on the CRI pod sandbox image (#404).
func TestRunCacheWarmCheckShallowAlsoCoversTheSandboxImage(t *testing.T) {
	t.Setenv("HOME", shortTestHome(t))
	socketPath, err := daemon.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	listPath := filepath.Join(t.TempDir(), "images.txt")
	ref := "registry.k8s.io/kube-apiserver:v1.36.2"
	if err := os.WriteFile(listPath, []byte(ref+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	wantRefs := append([]string{ref}, provision.BootstrapRequiredImages()...)
	done := make(chan struct{})
	go serveDaemonRequests(t, listener, len(wantRefs), func(index int, request daemon.Request) daemon.Response {
		var args daemon.CacheCheckArgs
		if err := json.Unmarshal(request.Args, &args); err != nil {
			t.Fatal(err)
		}
		if args.Deep {
			t.Fatal("cache.check deep = true, want false")
		}
		if len(args.Refs) != 1 || args.Refs[0] != wantRefs[index] {
			t.Fatalf("refs = %v, want [%q]", args.Refs, wantRefs[index])
		}
		if args.Refs[0] == provision.KubernetesSandboxImage {
			return daemon.Response{OK: true, Data: mustJSON(t, daemon.CacheCheckResult{
				Entries: []daemon.CacheCheckEntry{{
					Ref: provision.KubernetesSandboxImage, Status: daemon.CacheCheckStatusFailed, Reason: "manifest not cached",
				}},
				Failed: 1,
			})}
		}
		return daemon.Response{OK: true, Data: mustJSON(t, daemon.CacheCheckResult{
			Entries:  []daemon.CacheCheckEntry{{Ref: args.Refs[0], Status: daemon.CacheCheckStatusComplete}},
			Complete: 1,
		})}
	}, done)

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	err = command.run([]string{"cache", "warm", "--check", listPath})
	<-done
	if err == nil || err.Error() != "cache check failed for 1 ref(s)" {
		t.Fatalf("shallow check err = %v, want cache check failed for 1 ref(s)", err)
	}
	if !strings.Contains(stdout.String(), "\u2717 "+provision.KubernetesSandboxImage+" manifest not cached") {
		t.Fatalf("shallow check did not flag the missing sandbox image: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "is the CRI pod sandbox image every node needs") {
		t.Fatalf("shallow check did not name the remedy: %q", stdout.String())
	}
}

func TestRunCacheWarmCheckReadsFilesAndStdinThenPrintsMixedSummary(t *testing.T) {
	t.Setenv("HOME", shortTestHome(t))
	socketPath, err := daemon.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	dir := t.TempDir()
	firstList := filepath.Join(dir, "images-a.txt")
	secondList := filepath.Join(dir, "images-b.txt")
	firstRef := "docker.io/library/pause:3.10"
	secondRef := "public.ecr.aws/eks-distro/kubernetes/pause:3.10"
	thirdRef := "ghcr.io/siderolabs/installer:v1.9.3"
	if err := os.WriteFile(firstList, []byte(firstRef+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondList, []byte(secondRef+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	wantRefs := append([]string{firstRef, secondRef, thirdRef}, provision.BootstrapRequiredImages()...)
	done := make(chan struct{})
	go serveDaemonRequests(t, listener, len(wantRefs), func(index int, request daemon.Request) daemon.Response {
		if request.Op != "cache.check" {
			t.Fatalf("request op = %q, want cache.check", request.Op)
		}
		var args daemon.CacheCheckArgs
		if err := json.Unmarshal(request.Args, &args); err != nil {
			t.Fatal(err)
		}
		if len(args.Refs) != 1 || args.Refs[0] != wantRefs[index] {
			t.Fatalf("refs = %v, want [%q]", args.Refs, wantRefs[index])
		}
		result := daemon.CacheCheckResult{Entries: []daemon.CacheCheckEntry{{Ref: wantRefs[index], Status: daemon.CacheCheckStatusComplete}}, Complete: 1}
		if index == 1 {
			result.Entries[0].Status = daemon.CacheCheckStatusFailed
			result.Entries[0].Reason = "blob sha256:deadbeef not cached"
			result.Complete = 0
			result.Failed = 1
		}
		return daemon.Response{OK: true, Data: mustJSON(t, result)}
	}, done)

	var stdout, stderr bytes.Buffer
	command := cli{
		out: &stdout,
		err: &stderr,
		in:  bytes.NewBufferString(thirdRef + "\n"),
	}
	err = command.run([]string{"cache", "warm", firstList, "--check", secondList, "-"})
	<-done
	if err == nil || err.Error() != "cache check failed for 1 ref(s)" {
		t.Fatalf("err = %v, want cache check failed for 1 ref(s)", err)
	}
	wantStdout := "" +
		"\u2713 docker.io/library/pause:3.10 complete\n" +
		"\u2717 public.ecr.aws/eks-distro/kubernetes/pause:3.10 blob sha256:deadbeef not cached\n" +
		"\u2713 ghcr.io/siderolabs/installer:v1.9.3 complete\n" +
		"\u2713 " + provision.KubernetesSandboxImage + " complete\n" +
		"summary: 3 complete, 1 failed\n"
	if got := stdout.String(); got != wantStdout {
		t.Fatalf("stdout = %q, want %q", got, wantStdout)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunCacheWarmRejectsDeepWithoutCheck(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	err := command.run([]string{"cache", "warm", "--deep", "images.txt"})
	if err == nil || err.Error() != "cache warm --deep requires --check" {
		t.Fatalf("err = %v, want cache warm --deep requires --check", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestRunCacheWarmCheckReportsEveryRefBeforeFailing(t *testing.T) {
	t.Setenv("HOME", shortTestHome(t))
	socketPath, err := daemon.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	listPath := filepath.Join(t.TempDir(), "images.txt")
	refs := []string{
		"docker.io/library/pause:3.10",
		"ghcr.io/example/app@sha256:1111111111111111111111111111111111111111111111111111111111111111",
	}
	if err := os.WriteFile(listPath, []byte(strings.Join(refs, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	wantRefs := append(append([]string{}, refs...), provision.BootstrapRequiredImages()...)
	done := make(chan struct{})
	go serveDaemonRequests(t, listener, len(wantRefs), func(index int, request daemon.Request) daemon.Response {
		var args daemon.CacheCheckArgs
		if err := json.Unmarshal(request.Args, &args); err != nil {
			t.Fatal(err)
		}
		if len(args.Refs) != 1 || args.Refs[0] != wantRefs[index] {
			t.Fatalf("refs = %v, want [%q]", args.Refs, wantRefs[index])
		}
		result := daemon.CacheCheckResult{Entries: []daemon.CacheCheckEntry{{Ref: wantRefs[index], Status: daemon.CacheCheckStatusComplete}}, Complete: 1}
		if index == 0 {
			result.Entries[0].Status = daemon.CacheCheckStatusFailed
			result.Entries[0].Reason = "blob missing"
			result.Complete = 0
			result.Failed = 1
		}
		return daemon.Response{OK: true, Data: mustJSON(t, result)}
	}, done)

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	err = command.run([]string{"cache", "warm", "--check", listPath})
	<-done
	if err == nil || err.Error() != "cache check failed for 1 ref(s)" {
		t.Fatalf("err = %v, want cache check failed for 1 ref(s)", err)
	}
	wantStdout := "" +
		"\u2717 docker.io/library/pause:3.10 blob missing\n" +
		"\u2713 ghcr.io/example/app@sha256:1111111111111111111111111111111111111111111111111111111111111111 complete\n" +
		"\u2713 " + provision.KubernetesSandboxImage + " complete\n" +
		"summary: 2 complete, 1 failed\n"
	if got := stdout.String(); got != wantStdout {
		t.Fatalf("stdout = %q, want %q", got, wantStdout)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

// The deep check reports a gap in an image no warm list names, and `cache warm`
// itself never pulls it — so the report has to name the verb that does (#348).
func TestRunCacheWarmCheckDeepNamesTheRemedyForTheSandboxImage(t *testing.T) {
	t.Setenv("HOME", shortTestHome(t))
	socketPath, err := daemon.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	listPath := filepath.Join(t.TempDir(), "images.txt")
	ref := "docker.io/library/pause:3.10"
	if err := os.WriteFile(listPath, []byte(ref+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantRefs := append([]string{ref}, provision.BootstrapRequiredImages()...)
	done := make(chan struct{})
	go serveDaemonRequests(t, listener, len(wantRefs), func(index int, _ daemon.Request) daemon.Response {
		if index == 0 {
			return daemon.Response{OK: true, Data: mustJSON(t, daemon.CacheCheckResult{
				Entries:  []daemon.CacheCheckEntry{{Ref: wantRefs[index], Status: daemon.CacheCheckStatusComplete}},
				Complete: 1,
			})}
		}
		return daemon.Response{OK: true, Data: mustJSON(t, daemon.CacheCheckResult{
			Entries: []daemon.CacheCheckEntry{{Ref: wantRefs[index], Status: daemon.CacheCheckStatusFailed, Reason: "not cached"}},
			Failed:  1,
		})}
	}, done)

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	err = command.run([]string{"cache", "warm", "--check", "--deep", listPath})
	<-done
	if err == nil {
		t.Fatal("cache warm --check --deep succeeded despite the missing sandbox image")
	}
	if !strings.Contains(stdout.String(), "tbx cache pull") {
		t.Fatalf("stdout = %q, want it to name `tbx cache pull` as the remedy", stdout.String())
	}
	if !strings.Contains(stdout.String(), provision.KubernetesSandboxImage+" is the CRI pod sandbox image") {
		t.Fatalf("stdout = %q, want it to explain the sandbox image gap", stdout.String())
	}
}
