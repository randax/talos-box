package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/daemon"
)

func TestRunCacheWarmReadsFilesAndStdinThenPrintsSummary(t *testing.T) {
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
	go serveDaemonRequests(t, listener, len(wantRefs), func(index int, request daemon.Request) daemon.Response {
		if request.Op != "cache.warm" {
			t.Fatalf("request op = %q, want cache.warm", request.Op)
		}
		var args daemon.CacheWarmArgs
		if err := json.Unmarshal(request.Args, &args); err != nil {
			t.Fatal(err)
		}
		if len(args.Refs) != 1 || args.Refs[0] != wantRefs[index] {
			t.Fatalf("refs = %v, want [%q]", args.Refs, wantRefs[index])
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
		"summary: 2 warmed, 1 already complete, 0 failed\n"
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
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("tbxd protocol %d is too old", cacheWarmProtocolVersion-1)) || !strings.Contains(err.Error(), "restart or upgrade tbxd") {
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
		firstResult  any
		secondResult any
		firstOutput  string
		fullOutput   string
	}{
		{
			name:         "warm",
			op:           "cache.warm",
			firstResult:  daemon.CacheWarmResult{Entries: []daemon.CacheWarmEntry{{Ref: "docker.io/library/pause:3.10", Status: daemon.CacheWarmStatusWarmed}}, Warmed: 1},
			secondResult: daemon.CacheWarmResult{Entries: []daemon.CacheWarmEntry{{Ref: "ghcr.io/example/app:v1.0.0", Status: daemon.CacheWarmStatusAlreadyComplete}}, AlreadyComplete: 1},
			firstOutput:  "\u2713 docker.io/library/pause:3.10 warmed\n",
			fullOutput: "\u2713 docker.io/library/pause:3.10 warmed\n" +
				"\u2713 ghcr.io/example/app:v1.0.0 already complete\n" +
				"summary: 1 warmed, 1 already complete, 0 failed\n",
		},
		{
			name:         "check",
			checkOnly:    true,
			op:           "cache.check",
			firstResult:  daemon.CacheCheckResult{Entries: []daemon.CacheCheckEntry{{Ref: "docker.io/library/pause:3.10", Status: daemon.CacheCheckStatusComplete}}, Complete: 1},
			secondResult: daemon.CacheCheckResult{Entries: []daemon.CacheCheckEntry{{Ref: "ghcr.io/example/app:v1.0.0", Status: daemon.CacheCheckStatusComplete}}, Complete: 1},
			firstOutput:  "\u2713 docker.io/library/pause:3.10 complete\n",
			fullOutput: "\u2713 docker.io/library/pause:3.10 complete\n" +
				"\u2713 ghcr.io/example/app:v1.0.0 complete\n" +
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

			refs := []string{"docker.io/library/pause:3.10", "ghcr.io/example/app:v1.0.0"}
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

func TestRunCacheWarmReturnsErrorWhenAnyRefFails(t *testing.T) {
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
	go serveDaemonRequests(t, listener, len(refs), func(index int, request daemon.Request) daemon.Response {
		var args daemon.CacheWarmArgs
		if err := json.Unmarshal(request.Args, &args); err != nil {
			t.Fatal(err)
		}
		if len(args.Refs) != 1 || args.Refs[0] != refs[index] {
			t.Fatalf("refs = %v, want [%q]", args.Refs, refs[index])
		}
		result := daemon.CacheWarmResult{Entries: []daemon.CacheWarmEntry{{Ref: refs[index], Status: daemon.CacheWarmStatusWarmed}}, Warmed: 1}
		if index == 0 {
			result.Entries[0].Status = daemon.CacheWarmStatusFailed
			result.Entries[0].Reason = "upstream: timeout"
			result.Warmed = 0
			result.Failed = 1
		}
		return daemon.Response{OK: true, Data: mustJSON(t, result)}
	}, done)

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	err = command.run([]string{"cache", "warm", listPath})
	<-done
	if err == nil || err.Error() != "cache warm failed for 1 ref(s)" {
		t.Fatalf("err = %v, want cache warm failed for 1 ref(s)", err)
	}
	wantStdout := "" +
		"\u2717 docker.io/library/pause:3.10 upstream: timeout\n" +
		"\u2713 ghcr.io/example/app:v1.0.0 warmed\n" +
		"summary: 1 warmed, 0 already complete, 1 failed\n"
	if got := stdout.String(); got != wantStdout {
		t.Fatalf("stdout = %q, want %q", got, wantStdout)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}
