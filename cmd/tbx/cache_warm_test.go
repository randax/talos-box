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
	"slices"
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
	go serveWarmRequests(t, listener, wantRefs, func(index int, request daemon.Request) daemon.Response {
		if request.Op != "cache.warm" {
			t.Fatalf("request op = %q, want cache.warm", request.Op)
		}
		var args daemon.CacheWarmArgs
		if err := json.Unmarshal(request.Args, &args); err != nil {
			t.Fatal(err)
		}
		if args.Refresh {
			t.Error("Refresh = true, want false")
		}
		// the default 8 jobs are split across the 4 requests in flight
		if want := daemon.DefaultCacheWarmJobs / 4; args.Jobs != want {
			t.Errorf("Jobs = %d, want the default's per-request share %d", args.Jobs, want)
		}
		result := daemon.CacheWarmResult{Entries: []daemon.CacheWarmEntry{{Ref: wantRefs[index]}}}
		switch index {
		case 0, 2:
			result.Entries[0].Status = daemon.CacheWarmStatusWarmed
			result.Warmed = 1
		case 1:
			result.Entries[0].Status = daemon.CacheWarmStatusAlreadyComplete
			result.AlreadyComplete = 1
		}
		return daemon.Response{OK: true, Data: mustJSON(t, daemon.CacheWarmResult{
			Entries:         result.Entries,
			Warmed:          result.Warmed,
			AlreadyComplete: result.AlreadyComplete,
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
			name:         "warm",
			op:           "cache.warm",
			refs:         []string{"docker.io/library/pause:3.10", "ghcr.io/example/app:v1.0.0"},
			firstResult:  daemon.CacheWarmResult{Entries: []daemon.CacheWarmEntry{{Ref: "docker.io/library/pause:3.10", Status: daemon.CacheWarmStatusWarmed}}, Warmed: 1},
			secondResult: daemon.CacheWarmResult{Entries: []daemon.CacheWarmEntry{{Ref: "ghcr.io/example/app:v1.0.0", Status: daemon.CacheWarmStatusAlreadyComplete}}, AlreadyComplete: 1},
			firstOutput:  "\u2713 docker.io/library/pause:3.10 warmed\n",
			fullOutput: "\u2713 docker.io/library/pause:3.10 warmed\n" +
				"\u2713 ghcr.io/example/app:v1.0.0 already complete\n" +
				"summary: 1 warmed, 1 already complete, 0 failed (missing)\n",
		},
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
			} else {
				// the stub answers by arrival order; --jobs 1 keeps one
				// request in flight so arrival order is list order
				args = append(args, "--jobs", "1")
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

// serveWarmRequests is serveDaemonRequests for `cache.warm`, whose requests
// arrive a few at a time in no fixed order since #506: the index handed to
// respond is the ref's position in `refs`, not the arrival order.
func serveWarmRequests(t *testing.T, listener net.Listener, refs []string, respond func(int, daemon.Request) daemon.Response, done chan<- struct{}) {
	t.Helper()
	serveDaemonRequests(t, listener, len(refs), func(_ int, request daemon.Request) daemon.Response {
		var args daemon.CacheWarmArgs
		if err := json.Unmarshal(request.Args, &args); err != nil {
			t.Error(err)
			return daemon.Response{Error: err.Error()}
		}
		if len(args.Refs) != 1 {
			t.Errorf("refs = %v, want exactly one", args.Refs)
			return daemon.Response{Error: "want one ref"}
		}
		index := slices.Index(refs, args.Refs[0])
		if index < 0 {
			t.Errorf("unexpected ref %q", args.Refs[0])
			return daemon.Response{Error: "unexpected ref"}
		}
		return respond(index, request)
	}, done)
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
	go serveWarmRequests(t, listener, refs, func(index int, request daemon.Request) daemon.Response {
		var args daemon.CacheWarmArgs
		if err := json.Unmarshal(request.Args, &args); err != nil {
			t.Fatal(err)
		}
		if len(args.Refs) != 1 || args.Refs[0] != refs[index] {
			t.Fatalf("refs = %v, want [%q]", args.Refs, refs[index])
		}
		result := daemon.CacheWarmResult{Failed: 1}
		if index == 0 {
			result.Entries = []daemon.CacheWarmEntry{{Ref: refs[index], Status: daemon.CacheWarmStatusFailedMissing, Reason: "upstream: timeout"}}
			result.FailedMissing = 1
		} else {
			result.Entries = []daemon.CacheWarmEntry{{Ref: refs[index], Status: daemon.CacheWarmStatusFailedRevalidate, Reason: "upstream 404"}}
			result.FailedRevalidate = 1
		}
		return daemon.Response{OK: true, Data: mustJSON(t, result)}
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

func TestRunCacheWarmRejectsJobsBelowOne(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	err := command.run([]string{"cache", "warm", "--jobs", "0", "images.txt"})
	if err == nil || err.Error() != "cache warm --jobs must be at least 1, got 0" {
		t.Fatalf("err = %v, want --jobs usage error", err)
	}
}

// warmStubListener serves the protocol handshake and then hands every
// cache.warm connection to serve, concurrently, until the listener closes.
func warmStubListener(t *testing.T, serve func(net.Conn, daemon.CacheWarmArgs)) net.Listener {
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
			go func() {
				defer func() { _ = connection.Close() }()
				var request daemon.Request
				if err := json.NewDecoder(connection).Decode(&request); err != nil {
					return
				}
				if request.Op == "daemon.info" {
					_ = json.NewEncoder(connection).Encode(daemon.Response{OK: true, Data: mustJSON(t, daemon.Info{ProtocolVersion: daemon.ProtocolVersion})})
					return
				}
				var args daemon.CacheWarmArgs
				if err := json.Unmarshal(request.Args, &args); err != nil || request.Op != "cache.warm" || len(args.Refs) != 1 {
					t.Errorf("request = %+v (%v), want one-ref cache.warm", request, err)
					return
				}
				serve(connection, args)
			}()
		}
	}()
	return listener
}

func warmedResponse(t *testing.T, ref string) daemon.Response {
	t.Helper()
	return daemon.Response{OK: true, Data: mustJSON(t, daemon.CacheWarmResult{
		Entries: []daemon.CacheWarmEntry{{Ref: ref, Status: daemon.CacheWarmStatusWarmed}},
		Warmed:  1,
	})}
}

func writeWarmList(t *testing.T, refs []string) string {
	t.Helper()
	listPath := filepath.Join(t.TempDir(), "images.txt")
	if err := os.WriteFile(listPath, []byte(strings.Join(refs, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return listPath
}

func TestRunCacheWarmKeepsSeveralRefsInFlightAndPrintsInListOrder(t *testing.T) {
	refs := []string{
		"registry.example/one:v1",
		"registry.example/two:v1",
		"registry.example/three:v1",
		"registry.example/four:v1",
		"registry.example/five:v1",
		"registry.example/six:v1",
	}
	var mu sync.Mutex
	inFlight, peak := 0, 0
	holdFirst := make(chan struct{})
	var shares []int
	warmStubListener(t, func(connection net.Conn, args daemon.CacheWarmArgs) {
		mu.Lock()
		shares = append(shares, args.Jobs)
		mu.Unlock()
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		if args.Refs[0] == refs[0] {
			<-holdFirst
		} else {
			// let the others pile up while the first is held
			time.Sleep(50 * time.Millisecond)
		}
		mu.Lock()
		inFlight--
		mu.Unlock()
		_ = json.NewEncoder(connection).Encode(warmedResponse(t, args.Refs[0]))
	})

	stdout := newSignalBuffer()
	var stderr bytes.Buffer
	command := cli{out: stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	commandDone := make(chan error, 1)
	go func() { commandDone <- command.run([]string{"cache", "warm", "--jobs", "6", writeWarmList(t, refs)}) }()
	// --jobs 6 over 4 requests in flight: shares of 2, 2, 1, 1

	// the later refs finish while the first is held, yet nothing prints:
	// output is in list order
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		p := peak
		mu.Unlock()
		if p >= 4 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := stdout.String(); got != "" {
		t.Fatalf("printed %q while the first ref was still in flight", got)
	}
	close(holdFirst)
	if err := <-commandDone; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if peak < 2 || peak > 4 {
		t.Fatalf("peak requests in flight = %d, want parallel and at most min(4, jobs) = 4", peak)
	}
	for _, share := range shares {
		if share != 1 && share != 2 {
			t.Fatalf("request shares = %v, want each request to carry 1 or 2 of the 6 jobs", shares)
		}
	}
	var want strings.Builder
	for _, ref := range refs {
		want.WriteString("✓ " + ref + " warmed\n")
	}
	want.WriteString("summary: 6 warmed, 0 already complete, 0 failed (missing)\n")
	if got := stdout.String(); got != want.String() {
		t.Fatalf("stdout = %q, want %q", got, want.String())
	}
}

func TestWarmRefsConcurrentlySplitsJobsAcrossRequests(t *testing.T) {
	for jobs, want := range map[int][]int{1: {1}, 2: {1, 1}, 3: {1, 1, 1}, 4: {1, 1, 1, 1}, 6: {2, 2, 1, 1}, 8: {2, 2, 2, 2}, 9: {3, 2, 2, 2}} {
		got := warmJobShares(jobs)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("shares(%d) = %v, want %v", jobs, got, want)
		}
	}
}

func TestRunCacheWarmJobsOneKeepsOneRequestInFlight(t *testing.T) {
	refs := []string{"registry.example/one:v1", "registry.example/two:v1", "registry.example/three:v1"}
	var mu sync.Mutex
	inFlight, peak := 0, 0
	warmStubListener(t, func(connection net.Conn, args daemon.CacheWarmArgs) {
		if args.Jobs != 1 {
			t.Errorf("Jobs = %d, want 1", args.Jobs)
		}
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		_ = json.NewEncoder(connection).Encode(warmedResponse(t, args.Refs[0]))
	})
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	if err := command.run([]string{"cache", "warm", "--jobs", "1", writeWarmList(t, refs)}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if peak != 1 {
		t.Fatalf("peak requests in flight = %d, want 1", peak)
	}
}

func TestRunCacheWarmStopsStartingRequestsAfterAnError(t *testing.T) {
	refs := make([]string, 12)
	for i := range refs {
		refs[i] = fmt.Sprintf("registry.example/img%d:v1", i)
	}
	var mu sync.Mutex
	seen := map[string]bool{}
	warmStubListener(t, func(connection net.Conn, args daemon.CacheWarmArgs) {
		mu.Lock()
		seen[args.Refs[0]] = true
		mu.Unlock()
		if args.Refs[0] == refs[0] {
			_ = json.NewEncoder(connection).Encode(daemon.Response{Error: "daemon exploded"})
			return
		}
		time.Sleep(20 * time.Millisecond)
		_ = json.NewEncoder(connection).Encode(warmedResponse(t, args.Refs[0]))
	})
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	err := command.run([]string{"cache", "warm", writeWarmList(t, refs)})
	if err == nil || !strings.Contains(err.Error(), "daemon exploded") {
		t.Fatalf("err = %v, want the daemon error", err)
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) > 4 {
		t.Fatalf("%d refs were requested after the first failed, want at most the 4 already in flight", len(seen))
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing after a failed first ref", stdout.String())
	}
}
