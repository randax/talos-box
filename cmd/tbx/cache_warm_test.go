package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/provision"
)

func TestRunCacheWarmRejectsRefreshWithCheck(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	err := command.run([]string{"cache", "warm", "--refresh", "--check", "images.txt"})
	if err == nil || err.Error() != "cache warm --refresh cannot be used with --check" {
		t.Fatalf("err = %v, want refresh/check usage error", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestRunCacheWarmHelpExplainsCacheFirstRefreshAndCompleteness(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	err := command.run([]string{"cache", "warm", "--help"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("err = %v, want flag.ErrHelp", err)
	}
	for _, want := range []string{
		"makes no upstream request for complete refs",
		"--refresh revalidates complete unpinned tags",
		"digest-pinned refs do not need freshness resolution",
		"transient refresh failure is nonfatal",
		"selected linux/<arch> manifest, config, and all layers locally",
		"--jobs N keeps N blob downloads in flight",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("help = %q, want %q", stderr.String(), want)
		}
	}
}

func TestRunCacheWarmDefaultDoesNotRequestRefresh(t *testing.T) {
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

	first := filepath.Join(t.TempDir(), "one.txt")
	second := filepath.Join(t.TempDir(), "two.txt")
	if err := os.WriteFile(first, []byte("\n# ignored\ndocker.io/library/pause:3.10\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("ghcr.io/example/app@sha256:1111111111111111111111111111111111111111111111111111111111111111\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	wantRefs := []string{
		"docker.io/library/pause:3.10",
		"public.ecr.aws/eks-distro/kubernetes/pause:3.10",
		"ghcr.io/example/app@sha256:1111111111111111111111111111111111111111111111111111111111111111",
	}
	done := make(chan struct{})
	go serveDaemonRequests(t, listener, 1, func(_ int, request daemon.Request) daemon.Response {
		if request.Op != "cache.warm" {
			t.Errorf("request op = %q, want cache.warm", request.Op)
		}
		if !request.Progress {
			t.Error("Progress = false, want narration requested")
		}
		var args daemon.CacheWarmArgs
		if err := json.Unmarshal(request.Args, &args); err != nil {
			t.Error(err)
		}
		if fmt.Sprint(args.Refs) != fmt.Sprint(wantRefs) {
			t.Errorf("refs = %v, want the whole list %v in one request", args.Refs, wantRefs)
		}
		if args.Refresh {
			t.Error("Refresh = true, want false")
		}
		if args.Jobs != daemon.DefaultCacheWarmJobs {
			t.Errorf("Jobs = %d, want the default %d", args.Jobs, daemon.DefaultCacheWarmJobs)
		}
		return daemon.Response{OK: true, Data: mustJSON(t, daemon.CacheWarmResult{
			Entries: []daemon.CacheWarmEntry{
				{Ref: wantRefs[0], Status: daemon.CacheWarmStatusWarmed},
				{Ref: wantRefs[1], Status: daemon.CacheWarmStatusAlreadyComplete},
				{Ref: wantRefs[2], Status: daemon.CacheWarmStatusWarmed},
			},
			Warmed:          2,
			AlreadyComplete: 1,
		})}
	}, done)

	var stdout, stderr bytes.Buffer
	command := cli{
		out: &stdout,
		err: &stderr,
		in:  bytes.NewBufferString("\n# ignored\npublic.ecr.aws/eks-distro/kubernetes/pause:3.10\n"),
	}
	if err := command.run([]string{"cache", "warm", first, "-", second}); err != nil {
		t.Fatal(err)
	}
	<-done

	wantStdout := "" +
		"\u2713 docker.io/library/pause:3.10 warmed\n" +
		"\u2713 public.ecr.aws/eks-distro/kubernetes/pause:3.10 already complete\n" +
		"\u2713 ghcr.io/example/app@sha256:1111111111111111111111111111111111111111111111111111111111111111 warmed\n" +
		"summary: 2 warmed, 1 already complete, 0 failed (missing)\n"
	if got := stdout.String(); got != wantStdout {
		t.Fatalf("stdout = %q, want %q", got, wantStdout)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunCacheWarmRejectsInvalidRefsBeforeDaemonCall(t *testing.T) {
	listPath := filepath.Join(t.TempDir(), "images.txt")
	if err := os.WriteFile(listPath, []byte(strings.Join([]string{
		"# workshop images",
		"docker.io/library/nginx:1.27.0",
		"docker.io/library/nginx:latest",
		"docker.io/library/nginx@sha256:not-hex",
		"docker.io/library/repo:@sha256:1111111111111111111111111111111111111111111111111111111111111111",
		"docker.io/:tag",
		"docker.io/library/nginx:1.2.3?query",
		"docker.io/library/nginx:1.2.3#fragment",
		"docker.io/library/ bad:1.2.3",
		"registry.example/repo:one:two",
		"registry.example/Uppercase/repo:1.2.3",
		"registry.example/repo:-not-a-tag",
	}, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBufferString("")}
	err := command.run([]string{"cache", "warm", listPath})
	if err == nil {
		t.Fatal("run(cache warm) succeeded, want parse error")
	}
	if !strings.Contains(err.Error(), filepath.Base(listPath)+":3") || !strings.Contains(err.Error(), ":latest") {
		t.Fatalf("error = %q, want latest-tag source line rejection", err)
	}
	if !strings.Contains(err.Error(), filepath.Base(listPath)+":4") || !strings.Contains(err.Error(), "must use a sha256 or sha512 digest") {
		t.Fatalf("error = %q, want digest source line rejection", err)
	}
	for _, line := range []int{5, 6, 7, 8, 9, 10, 11, 12} {
		if !strings.Contains(err.Error(), filepath.Base(listPath)+":"+fmt.Sprint(line)) {
			t.Fatalf("error = %q, want malformed ref line %d", err, line)
		}
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestRunCacheWarmRejectsInvalidRegistryAuthoritiesBeforeDaemonCall(t *testing.T) {
	listPath := filepath.Join(t.TempDir(), "images.txt")
	longHost := strings.Repeat("a.", 126) + "aaaa"
	if err := os.WriteFile(listPath, []byte(strings.Join([]string{
		"registry.example:0/repo:tag",
		"registry.example:99999/repo:tag",
		strings.Repeat("a", 64) + ".example/repo:tag",
		longHost + "/repo:tag",
	}, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	err := command.run([]string{"cache", "warm", listPath})
	if err == nil {
		t.Fatal("run(cache warm) succeeded, want parse error")
	}
	for line := 1; line <= 4; line++ {
		if !strings.Contains(err.Error(), filepath.Base(listPath)+":"+fmt.Sprint(line)) {
			t.Fatalf("error = %q, want invalid authority at line %d", err, line)
		}
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestRunCacheWarmRefusesOldProtocolBeforeCacheRPC(t *testing.T) {
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
	if err := os.WriteFile(listPath, []byte("docker.io/library/pause:3.10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go serveSingleDaemonRequest(t, listener, func(request daemon.Request) daemon.Response {
		if request.Op != "daemon.info" {
			t.Fatalf("first operation = %q, want daemon.info", request.Op)
		}
		return daemon.Response{OK: true, Data: mustJSON(t, daemon.Info{ProtocolVersion: cacheWarmProtocolVersion - 1})}
	}, done)

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	err = command.run([]string{"cache", "warm", listPath})
	<-done
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("tbxd protocol %d is too old", cacheWarmProtocolVersion-1)) || !strings.Contains(err.Error(), "run: tbx system restart") {
		t.Fatalf("err = %v, want old-daemon restart/upgrade guidance", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestRunCacheWarmEmitsEachResultAsItCompletes(t *testing.T) {
	for _, test := range []struct {
		name         string
		checkOnly    bool
		op           string
		refs         []string
		firstResult  any
		secondResult any
		firstOutput  string
		fullOutput   string
	}{
		{
			// A check also verifies the bootstrap-required set (#404), so the
			// list already names it here to keep the request count at two.
			name:         "check",
			checkOnly:    true,
			op:           "cache.check",
			refs:         []string{"docker.io/library/pause:3.10", provision.KubernetesSandboxImage},
			firstResult:  daemon.CacheCheckResult{Entries: []daemon.CacheCheckEntry{{Ref: "docker.io/library/pause:3.10", Status: daemon.CacheCheckStatusComplete}}, Complete: 1},
			secondResult: daemon.CacheCheckResult{Entries: []daemon.CacheCheckEntry{{Ref: provision.KubernetesSandboxImage, Status: daemon.CacheCheckStatusComplete}}, Complete: 1},
			firstOutput:  "\u2713 docker.io/library/pause:3.10 complete\n",
			fullOutput: "\u2713 docker.io/library/pause:3.10 complete\n" +
				"\u2713 " + provision.KubernetesSandboxImage + " complete\n" +
				"summary: 2 complete, 0 failed\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
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

			refs := test.refs
			listPath := filepath.Join(t.TempDir(), "images.txt")
			if err := os.WriteFile(listPath, []byte(strings.Join(refs, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			releaseSecond := make(chan struct{})
			serverDone := make(chan struct{})
			go func() {
				defer close(serverDone)
				connection, err := listener.Accept()
				if err != nil {
					t.Error(err)
					return
				}
				var infoRequest daemon.Request
				if err := json.NewDecoder(connection).Decode(&infoRequest); err != nil {
					t.Error(err)
					_ = connection.Close()
					return
				}
				if infoRequest.Op != "daemon.info" {
					t.Errorf("first operation = %q, want daemon.info", infoRequest.Op)
				}
				if err := json.NewEncoder(connection).Encode(daemon.Response{OK: true, Data: mustJSON(t, daemon.Info{ProtocolVersion: daemon.ProtocolVersion})}); err != nil {
					t.Error(err)
				}
				_ = connection.Close()
				for index, result := range []any{test.firstResult, test.secondResult} {
					connection, err := listener.Accept()
					if err != nil {
						t.Error(err)
						return
					}
					var request daemon.Request
					if err := json.NewDecoder(connection).Decode(&request); err != nil {
						t.Error(err)
						_ = connection.Close()
						return
					}
					if request.Op != test.op {
						t.Errorf("request op = %q, want %q", request.Op, test.op)
					}
					if test.checkOnly {
						var args daemon.CacheCheckArgs
						if err := json.Unmarshal(request.Args, &args); err != nil {
							t.Error(err)
						} else if args.Deep || len(args.Refs) != 1 || args.Refs[0] != refs[index] {
							t.Errorf("check args = %+v, want one ref %q and deep=false", args, refs[index])
						}
					} else {
						var args daemon.CacheWarmArgs
						if err := json.Unmarshal(request.Args, &args); err != nil {
							t.Error(err)
						} else if len(args.Refs) != 1 || args.Refs[0] != refs[index] {
							t.Errorf("warm args = %+v, want one ref %q", args, refs[index])
						}
					}
					if err := json.NewEncoder(connection).Encode(daemon.Response{OK: true, Data: mustJSON(t, result)}); err != nil {
						t.Error(err)
					}
					_ = connection.Close()
					if index == 0 {
						<-releaseSecond
					}
				}
			}()

			stdout := newSignalBuffer()
			var stderr bytes.Buffer
			command := cli{out: stdout, err: &stderr, in: bytes.NewBuffer(nil)}
			args := []string{"cache", "warm"}
			if test.checkOnly {
				args = append(args, "--check")
			}
			args = append(args, listPath)
			commandDone := make(chan error, 1)
			go func() { commandDone <- command.run(args) }()

			select {
			case <-stdout.wrote:
				if got := stdout.String(); got != test.firstOutput {
					t.Errorf("output before second result = %q, want %q", got, test.firstOutput)
				}
			case <-time.After(time.Second):
				t.Error("first result was not printed before the second request was allowed")
			}
			close(releaseSecond)
			if err := <-commandDone; err != nil {
				t.Fatal(err)
			}
			_ = listener.Close()
			<-serverDone

			if got := stdout.String(); got != test.fullOutput {
				t.Errorf("stdout = %q, want %q", got, test.fullOutput)
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

type signalBuffer struct {
	mu    sync.Mutex
	value strings.Builder
	wrote chan struct{}
	once  sync.Once
}

func newSignalBuffer() *signalBuffer {
	return &signalBuffer{wrote: make(chan struct{})}
}

func (b *signalBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.value.Write(value)
	b.once.Do(func() { close(b.wrote) })
	return len(value), nil
}

func (b *signalBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.value.String()
}

func serveDaemonRequests(t *testing.T, listener net.Listener, count int, respond func(int, daemon.Request) daemon.Response, done chan<- struct{}) {
	t.Helper()
	defer close(done)
	connection, err := listener.Accept()
	if err != nil {
		t.Error(err)
		return
	}
	var infoRequest daemon.Request
	if err := json.NewDecoder(connection).Decode(&infoRequest); err != nil {
		t.Error(err)
		_ = connection.Close()
		return
	}
	if infoRequest.Op != "daemon.info" {
		t.Errorf("first operation = %q, want daemon.info", infoRequest.Op)
	}
	if err := json.NewEncoder(connection).Encode(daemon.Response{OK: true, Data: mustJSON(t, daemon.Info{ProtocolVersion: daemon.ProtocolVersion})}); err != nil {
		t.Error(err)
	}
	_ = connection.Close()
	for index := 0; index < count; index++ {
		connection, err := listener.Accept()
		if err != nil {
			t.Error(err)
			return
		}
		var request daemon.Request
		if err := json.NewDecoder(connection).Decode(&request); err != nil {
			t.Error(err)
			_ = connection.Close()
			return
		}
		if err := json.NewEncoder(connection).Encode(respond(index, request)); err != nil {
			t.Error(err)
		}
		_ = connection.Close()
	}
}

func TestRunCacheWarmPrintsMissingAndRevalidateFailuresSeparately(t *testing.T) {
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
	refs := []string{"docker.io/library/pause:3.10", "ghcr.io/example/app:v1.0.0"}
	if err := os.WriteFile(listPath, []byte(strings.Join(refs, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go serveDaemonRequests(t, listener, 1, func(_ int, request daemon.Request) daemon.Response {
		var args daemon.CacheWarmArgs
		if err := json.Unmarshal(request.Args, &args); err != nil {
			t.Error(err)
		}
		if fmt.Sprint(args.Refs) != fmt.Sprint(refs) {
			t.Errorf("refs = %v, want %v", args.Refs, refs)
		}
		return daemon.Response{OK: true, Data: mustJSON(t, daemon.CacheWarmResult{
			Entries: []daemon.CacheWarmEntry{
				{Ref: refs[0], Status: daemon.CacheWarmStatusFailedMissing, Reason: "upstream: timeout"},
				{Ref: refs[1], Status: daemon.CacheWarmStatusFailedRevalidate, Reason: "upstream 404"},
			},
			Failed:           2,
			FailedMissing:    1,
			FailedRevalidate: 1,
		})}
	}, done)

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	err = command.run([]string{"cache", "warm", listPath})
	<-done
	if err == nil || err.Error() != "cache warm failed for 2 ref(s)" {
		t.Fatalf("err = %v, want cache warm failed for 2 ref(s)", err)
	}
	wantStdout := "" +
		"\u2717 docker.io/library/pause:3.10 failed (missing): upstream: timeout\n" +
		"\u2717 ghcr.io/example/app:v1.0.0 failed (revalidate): upstream 404\n" +
		"summary: 0 warmed, 0 already complete, 1 failed (missing), 1 failed (revalidate)\n"
	if got := stdout.String(); got != wantStdout {
		t.Fatalf("stdout = %q, want %q", got, wantStdout)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunCacheWarmRefreshSetsRefreshAndPrintsUnrevalidatedComplete(t *testing.T) {
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

	done := make(chan struct{})
	go serveDaemonRequests(t, listener, 1, func(_ int, request daemon.Request) daemon.Response {
		if request.Op != "cache.warm" {
			t.Fatalf("request op = %q, want cache.warm", request.Op)
		}
		var args daemon.CacheWarmArgs
		if err := json.Unmarshal(request.Args, &args); err != nil {
			t.Fatal(err)
		}
		if !args.Refresh {
			t.Fatal("Refresh = false, want true")
		}
		return daemon.Response{OK: true, Data: mustJSON(t, daemon.CacheWarmResult{
			Entries: []daemon.CacheWarmEntry{{
				Ref: ref, Status: daemon.CacheWarmStatusAlreadyComplete,
				RefreshWarning: "upstream 429, not revalidated",
			}},
			AlreadyComplete: 1,
		})}
	}, done)

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	if err := command.run([]string{"cache", "warm", "--refresh", listPath}); err != nil {
		t.Fatal(err)
	}
	<-done

	wantStdout := "" +
		"✓ docker.io/library/pause:3.10 already complete (upstream 429, not revalidated)\n" +
		"summary: 0 warmed, 1 already complete, 0 failed (missing), 0 failed (revalidate)\n" +
		"note: 1 complete ref(s) were not revalidated: docker.io/library/pause:3.10\n"
	if got := stdout.String(); got != wantStdout {
		t.Fatalf("stdout = %q, want %q", got, wantStdout)
	}
}

func TestRunCacheWarmOmitsTheRevalidateClauseWithoutRefresh(t *testing.T) {
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
	ref := "ghcr.io/example/app@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	if err := os.WriteFile(listPath, []byte(ref+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go serveDaemonRequests(t, listener, 1, func(_ int, request daemon.Request) daemon.Response {
		return daemon.Response{OK: true, Data: mustJSON(t, daemon.CacheWarmResult{
			Entries: []daemon.CacheWarmEntry{{Ref: ref, Status: daemon.CacheWarmStatusWarmed}},
			Warmed:  1,
		})}
	}, done)

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	if err := command.run([]string{"cache", "warm", listPath}); err != nil {
		t.Fatal(err)
	}
	<-done

	want := "✓ " + ref + " warmed\nsummary: 1 warmed, 0 already complete, 0 failed (missing)\n"
	if got := stdout.String(); got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestRunCacheWarmRejectsJobsOutsideItsRange(t *testing.T) {
	for _, jobs := range []string{"0", "-3", fmt.Sprint(daemon.MaxCacheWarmJobs + 1)} {
		var stdout, stderr bytes.Buffer
		command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
		err := command.run([]string{"cache", "warm", "--jobs", jobs, "images.txt"})
		if err == nil || !strings.HasPrefix(err.Error(), "cache warm --jobs must be between 1 and 16") {
			t.Fatalf("--jobs %s: err = %v, want range error", jobs, err)
		}
	}
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	err := command.run([]string{"cache", "warm", "--check", "--jobs", "4", "images.txt"})
	if err == nil || err.Error() != "cache warm --jobs cannot be used with --check" {
		t.Fatalf("err = %v, want jobs/check usage error", err)
	}
}

// warmNarrationListener serves the handshake, then one cache.warm request:
// it narrates the given entries as stages, waiting on each release channel
// before the next, and answers with the final result.
func warmNarrationListener(t *testing.T, narrate []daemon.CacheWarmEntry, release []<-chan struct{}, final daemon.CacheWarmResult, gotArgs chan<- daemon.CacheWarmArgs) net.Listener {
	t.Helper()
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
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			var request daemon.Request
			if err := json.NewDecoder(connection).Decode(&request); err != nil {
				_ = connection.Close()
				return
			}
			encoder := json.NewEncoder(connection)
			if request.Op == "daemon.info" {
				_ = encoder.Encode(daemon.Response{OK: true, Data: mustJSON(t, daemon.Info{ProtocolVersion: daemon.ProtocolVersion})})
				_ = connection.Close()
				continue
			}
			var args daemon.CacheWarmArgs
			if err := json.Unmarshal(request.Args, &args); err != nil || request.Op != "cache.warm" {
				t.Errorf("request = %+v (%v), want cache.warm", request, err)
			}
			gotArgs <- args
			for i, entry := range narrate {
				if i < len(release) && release[i] != nil {
					<-release[i]
				}
				_ = encoder.Encode(daemon.Response{Stage: daemon.CacheWarmEntryStagePrefix + string(mustJSON(t, entry))})
			}
			_ = encoder.Encode(daemon.Response{OK: true, Data: mustJSON(t, final)})
			_ = connection.Close()
		}
	}()
	return listener
}

func TestRunCacheWarmPrintsNarratedEntriesAsTheyArrive(t *testing.T) {
	refs := []string{"registry.example/one:v1", "registry.example/two:v1", "registry.example/three:v1"}
	entries := []daemon.CacheWarmEntry{
		{Ref: refs[0], Status: daemon.CacheWarmStatusWarmed},
		{Ref: refs[1], Status: daemon.CacheWarmStatusAlreadyComplete},
		{Ref: refs[2], Status: daemon.CacheWarmStatusFailedMissing, Reason: "upstream: timeout"},
	}
	final := daemon.CacheWarmResult{Entries: entries, Warmed: 1, AlreadyComplete: 1, Failed: 1, FailedMissing: 1}
	releaseSecond := make(chan struct{})
	releaseThird := make(chan struct{})
	gotArgs := make(chan daemon.CacheWarmArgs, 1)
	warmNarrationListener(t, entries, []<-chan struct{}{nil, releaseSecond, releaseThird}, final, gotArgs)

	listPath := filepath.Join(t.TempDir(), "images.txt")
	if err := os.WriteFile(listPath, []byte(strings.Join(refs, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout := newSignalBuffer()
	var stderr bytes.Buffer
	command := cli{out: stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	commandDone := make(chan error, 1)
	go func() { commandDone <- command.run([]string{"cache", "warm", "--jobs", "3", listPath}) }()

	select {
	case <-stdout.wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("first narrated entry was not printed before the second was released")
	}
	if got, want := stdout.String(), "\u2713 "+refs[0]+" warmed\n"; got != want {
		t.Fatalf("output after first entry = %q, want %q", got, want)
	}
	close(releaseSecond)
	close(releaseThird)
	err := <-commandDone
	if err == nil || err.Error() != "cache warm failed for 1 ref(s)" {
		t.Fatalf("err = %v, want one failed ref", err)
	}
	want := "\u2713 " + refs[0] + " warmed\n" +
		"\u2713 " + refs[1] + " already complete\n" +
		"\u2717 " + refs[2] + " failed (missing): upstream: timeout\n" +
		"summary: 1 warmed, 1 already complete, 1 failed (missing)\n"
	if got := stdout.String(); got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	args := <-gotArgs
	if args.Jobs != 3 || fmt.Sprint(args.Refs) != fmt.Sprint(refs) {
		t.Fatalf("args = %+v, want jobs 3 and the whole list", args)
	}
}

func TestRunCacheWarmPrintsEntriesADaemonDidNotNarrate(t *testing.T) {
	// an older daemon narrates nothing (or a newer one narrates part of the
	// list before a stage is lost); the final result covers the rest, once
	refs := []string{"registry.example/one:v1", "registry.example/two:v1"}
	entries := []daemon.CacheWarmEntry{
		{Ref: refs[0], Status: daemon.CacheWarmStatusWarmed},
		{Ref: refs[1], Status: daemon.CacheWarmStatusWarmed},
	}
	warmNarrationListener(t, entries[:1], nil, daemon.CacheWarmResult{Entries: entries, Warmed: 2}, make(chan daemon.CacheWarmArgs, 1))
	listPath := filepath.Join(t.TempDir(), "images.txt")
	if err := os.WriteFile(listPath, []byte(strings.Join(refs, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	if err := command.run([]string{"cache", "warm", listPath}); err != nil {
		t.Fatal(err)
	}
	want := "\u2713 " + refs[0] + " warmed\n" + "\u2713 " + refs[1] + " warmed\n" + "summary: 2 warmed, 0 already complete, 0 failed (missing)\n"
	if got := stdout.String(); got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestRunCacheWarmRejectsNarrationOutOfListOrder(t *testing.T) {
	refs := []string{"registry.example/one:v1", "registry.example/two:v1"}
	entries := []daemon.CacheWarmEntry{
		{Ref: refs[1], Status: daemon.CacheWarmStatusWarmed},
		{Ref: refs[0], Status: daemon.CacheWarmStatusWarmed},
	}
	warmNarrationListener(t, entries, nil, daemon.CacheWarmResult{Entries: entries, Warmed: 2}, make(chan daemon.CacheWarmArgs, 1))
	listPath := filepath.Join(t.TempDir(), "images.txt")
	if err := os.WriteFile(listPath, []byte(strings.Join(refs, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	err := command.run([]string{"cache", "warm", listPath})
	if err == nil || !strings.Contains(err.Error(), "narrated "+refs[1]+", want "+refs[0]) {
		t.Fatalf("err = %v, want out-of-order narration rejected", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing printed for misordered narration", stdout.String())
	}
}
