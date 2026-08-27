# Linux helper state ownership and doctor fixes — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the packaged Linux `tbx-helper` work as shipped (#467) and stop `tbx doctor` execing macOS tools / printing macOS remediation on Linux (#468).

**Architecture:** `tbxd` pushes cluster reservations to the helper over a new `net.sync` op (protocol v5); the helper persists them under `$STATE_DIRECTORY` and reconverges from that copy at startup. The helper never reads a user home again. Doctor probes gain per-GOOS seams.

**Tech Stack:** Go 1.26, netlink/nftables (existing), systemd units in `packaging/linux`.

**Spec:** `docs/superpowers/specs/2026-08-27-linux-helper-state-and-doctor-design.md`

## Global Constraints

- Tests must be hermetic (no network, no `factory.talos.dev`); `nix flake check` runs them offline.
- Lint per GOOS: `golangci-lint run ./...` and `GOOS=linux golangci-lint run ./...` before push.
- Daemon tests stub host memory via `TestMain` seams; new host probes go through seams.
- Protocol version bump → also bump the literal in `nix/vm-test.nix` (two places).
- Never print `sudo tbx system install` on Linux.

---

### Task 1: Helper startup — activated FD before socket-path validation (#467 d1)

**Files:** Modify `cmd/tbx-helper/main.go:44-58`; Test `cmd/tbx-helper/main_test.go`.

**Interfaces:** Produces `func openHelperListener(inherited func(string) (net.Listener, bool, error), resolve func() (string, error), listen func(string) (net.Listener, error)) (net.Listener, string, bool, error)`.

- [ ] Test: `TestOpenHelperListenerSkipsPathResolutionWhenActivated` — `inherited` returns a fake listener + `true`; `resolve` returns an error; expect no error, `activated=true`. Second case: not activated → `resolve` called, `listen` called with its path.
- [ ] Run `go test ./cmd/tbx-helper/ -run TestOpenHelperListener` → FAIL (undefined).
- [ ] Implement `openHelperListener` and use it in `run()`; `defer os.Remove` only when `!activated`.
- [ ] Run tests → PASS. Commit: `tbx-helper: use the activated listener before resolving the socket path (#467)`.

### Task 2: Helper state model + persistence

**Files:** Create `internal/helper/state.go`, `internal/helper/state_test.go`.

**Interfaces:** Produces
```go
type SyncedCluster struct { Name string `json:"name"`; SubnetIndex int `json:"subnetIndex"`; Nodes []SyncedNode `json:"nodes"` }
type SyncedNode struct { MAC string `json:"mac"`; IP string `json:"ip"` }
type helperState struct { mu sync.RWMutex; clusters []cluster.Cluster; path string }
func newHelperState(dir string) *helperState           // dir=="" → memory only
func (s *helperState) Load() error                     // missing file = empty
func (s *helperState) Replace(in []SyncedCluster) error // validates via cluster.NewReservationTable, persists atomically (temp+rename, 0600)
func (s *helperState) Clusters() []cluster.Cluster
func (s *helperState) SubnetIndexes() []int
func helperStateDir(getenv func(string) string) string // STATE_DIRECTORY, else TBX_HELPER_STATE_DIR, else ""
```
Conversion `SyncedCluster → cluster.Cluster` fills `Name, SubnetIndex, Nodes[{Name: "", MAC, IP}]` (only MAC/IP/SubnetIndex are read by `NewReservationTable`; node names are carried too so errors are readable — include `Name string \`json:"name"\`` in `SyncedNode`).

- [ ] Tests: round-trip Replace→Load in a `t.TempDir()`; Replace rejects duplicate MAC; memory-only state (dir "") works; `helperStateDir` precedence.
- [ ] Run → FAIL. Implement. Run → PASS. Commit: `helper: add synced reservation state with optional persistence (#467)`.

### Task 3: Helper uses synced state instead of cluster.List

**Files:** Modify `internal/helper/network_linux.go:65-77` (`configuredLinuxSubnetIndexes`), `internal/helper/dhcp_linux.go:41-48` (`load`), `internal/helper/networking.go`, `internal/helper/server.go:44-75`, `cmd/tbx-helper/main.go`. Tests: `internal/helper/dhcp_linux_test.go`, `internal/helper/server_test.go`.

**Interfaces:** `NewServer` gains the state: `func NewServer(state *helperState, allowedUID *uint32, allowAnyUID ...bool) *Server` (update all callers/tests; `nil` state → `newHelperState("")`). `newPlatformDHCPManager(load func() ([]cluster.Cluster, error))`. `ConvergeNetworking(subnets []int) error` replaces the zero-arg version; `main.go` calls `state.Load()` then `helper.ConvergeNetworking(state.SubnetIndexes())`.

Desired DHCP subnet set = `normalizeLinuxSubnetIndexes(state.SubnetIndexes() ∪ attached subnet indexes)`. Track attachments' subnet index: add `subnetIndex int` to `platformAttachment` set in `attach`, and give `linuxDHCPManager` a `extra func() []int` seam supplied by the server (`s.attachedSubnetIndexes()`).

- [ ] Test (dhcp_linux_test): manager with `load` returning one cluster on subnet 0 and `extra` returning `[3]` starts listeners for 0 and 3.
- [ ] Test (server_test): `net.attach` on subnet 5 with empty synced state still converges DHCP for 5 (fake manager records desired set).
- [ ] grep: `grep -rn 'cluster\.List' internal/helper cmd/tbx-helper` must return nothing.
- [ ] Run → FAIL, implement, PASS. Commit: `helper: converge from synced state, never from the helper's home (#467)`.

### Task 4: `net.sync` op, protocol v5, client Sync

**Files:** Modify `internal/helper/protocol.go` (version 5 + doc comment), `internal/helper/server.go` dispatch, `internal/helper/client.go`, `nix/vm-test.nix:50,55`. Tests: `internal/helper/server_test.go`, `internal/helper/client_test.go`, `internal/helper/protocol_test.go`.

**Interfaces:** Op `"net.sync"`, args `{"clusters":[SyncedCluster]}`, reply `{}`. `func (c *Client) Sync(clusters []cluster.Cluster) error` (converts to `[]SyncedCluster`). Server handler `func (s *Server) sync(raw json.RawMessage) (any, int, func(), error)` under `opMu`: `state.Replace`, then `convergeNetworking(state.SubnetIndexes())` on Linux (no-op elsewhere via `platformConvergeNetworking`), then `dhcp.Converge()`.

- [ ] Tests: sync with invalid reservation → error response, state unchanged; valid sync → fake dhcp manager saw Converge with subnet set; client `Sync` encodes MACs/IPs; protocol version test expects 5.
- [ ] Run → FAIL, implement, PASS. `grep -n 4 nix/vm-test.nix` updated to 5. Commit: `helper: add net.sync (protocol v5) so tbxd pushes reservations (#467)`.

### Task 5: tbxd pushes state

**Files:** Create `internal/daemon/helper_sync.go` + `_test.go`; modify `internal/daemon/operations.go` (after `cluster.Save` at ~629, ~1320, ~1390; after `cluster.Destroy` at ~1063), `internal/daemon/updown.go:373`, `internal/daemon/bgp.go:263`, `cmd/tbxd/networking.go` (call on startup).

**Interfaces:**
```go
// helper_sync.go
type helperSyncClient interface { Sync([]cluster.Cluster) error; Close() error }
var connectSyncHelper = func() (helperSyncClient, error) { return helper.Connect() }
var listClustersForSync = cluster.List
func syncHelperState() error  // list → connect → Sync → close; wraps errors "sync helper state: %w"
```
On create/start/up paths: `if err := syncHelperState(); err != nil { return ..., err }` **before** the first node attach. On destroy/stop/down/bgp paths: log only.

- [ ] Test: with a fake client, `syncHelperState` sends exactly what `listClustersForSync` returns; connection error is wrapped. Test in `operations_test.go` that create calls sync before attach (record order via seams).
- [ ] Run → FAIL, implement, PASS. Commit: `tbxd: push cluster reservations to the helper on start and every state change (#467)`.

### Task 6: Packaging + docs + e2e

**Files:** Modify `packaging/linux/usr/lib/systemd/system/tbx-helper.service` (add `StateDirectory=tbx`, `StateDirectoryMode=0700`), `docs/linux.md` (helper state section, `/var/lib/tbx`, protocol mismatch advice unchanged), `scripts/ci/linux-kvm-storage-e2e.sh:127` (systemd branch), `CONTEXT.md` if a term lands ("synced reservations").

- [ ] e2e script: `if command -v systemctl >/dev/null && systemctl is-system-running --quiet 2>/dev/null || [[ "$(systemctl is-system-running 2>/dev/null)" == degraded ]]; then install units + binary to /usr/bin, `sudo systemctl daemon-reload && sudo systemctl enable --now tbx-helper.socket`, `helper_socket=/var/run/tbx-helper.sock`, add `$USER` to `tbx` via `sg tbx` for the tbx invocations; else echo "packaged-unit coverage skipped: no systemd" and keep the direct launch.` Cleanup stops/disables the units.
- [ ] `shellcheck scripts/ci/linux-kvm-storage-e2e.sh`. Commit: `packaging: give tbx-helper a state directory; e2e drives the packaged units when systemd is present (#467)`.

### Task 7: Doctor routes per GOOS (#468.1)

**Files:** Modify `cmd/tbx/doctor_routes.go`; create `cmd/tbx/doctor_routes_darwin.go`, `cmd/tbx/doctor_routes_linux.go`, `cmd/tbx/doctor_routes_other.go`; tests `cmd/tbx/doctor_routes_test.go` (new, shared parser tests + linux `ip route` parser).

**Interfaces:** `checkClusterRoutes(clusters, statuses, probe routeProbe) error` where
```go
type routeProbe struct { iface func(ip string) (string, error); clusterIface func(string) bool; loopback string }
func platformRouteProbe(command commandOutput) routeProbe   // per-GOOS file
func parseIPRouteInterface(output []byte) (string, error)   // "172.30.0.1 dev br-tbx0 src ..." → br-tbx0; "local 172.30.0.1 dev lo ..." → lo
```
Linux command: `ip -o route get <ip>`; cluster iface prefix `br-tbx`; loopback `lo`. Darwin unchanged.

- [ ] Tests: `parseIPRouteInterface` cases; `checkClusterRoutes` with a fake probe passes for `br-tbx0`, fails for `wlan0`, accepts loopback for the gateway only.
- [ ] Update the existing callers/tests in `doctor.go`/`doctor_test.go`. Run → PASS both `go test ./cmd/tbx/` and `GOOS=linux go vet ./cmd/tbx/`. Commit: `doctor: probe host routes with ip route on Linux (#468)`.

### Task 8: security-inventory darwin-only (#468.2)

**Files:** Rename `cmd/tbx/doctor_security.go` → `doctor_security_darwin.go` (add `//go:build darwin`), create `doctor_security_other.go` returning `nil`, move its tests to `_darwin_test.go`. Verify `docs/linux.md` doctor table does not list `security-inventory`.

- [ ] `GOOS=linux go test ./cmd/tbx/` compiles and no `security-inventory` finding appears in the Linux findings test (`doctor_linux_test.go` — add an assertion). Commit: `doctor: keep the system-extension inventory macOS-only (#468)`.

### Task 9: GOOS-aware helper-unavailable advice (#468.3)

**Files:** Modify `internal/helper/protocol.go` (add `UnavailableAdvice()`), `internal/daemon/operations.go:878`, `cmd/tbxd/networking.go:44`, `cmd/tbx-helper/privileges_other.go` (already `!linux` — leave), tests `internal/helper/protocol_mismatch_test.go`.

**Interfaces:** `func UnavailableAdvice() string` → linux: ``enable the helper: `sudo systemctl enable --now tbx-helper.socket` and add your user to the `tbx` group (docs/linux.md)``; other: ``run `sudo tbx system install```. Internally `unavailableAdviceForGOOS(goos string) string` for tests.

- [ ] Test both branches; daemon `helperInstallError` message contains the advice. Commit: `helper: platform-correct advice when the helper is unreachable (#468)`.

### Task 10: KVM group remediation aware of WSL / lingering sessions (#468.3)

**Files:** Modify `cmd/tbx/doctor_platform_linux.go:234-248` and its deps struct (add `readFile` use for `/proc/sys/kernel/osrelease`); tests `cmd/tbx/doctor_linux_test.go`.

**Interfaces:** `func linuxSessionRefreshHint(osrelease string) string`; `linuxKVMFinding(accessRW func(string) error, osrelease string)`.

- [ ] Tests: osrelease `5.15.167.4-microsoft-standard-WSL2` → hint contains `wsl --shutdown`; `6.8.0-45-generic` → contains `log out and back in` and `loginctl terminate-user`. Commit: `doctor: name the real session-refresh step for the kvm group under WSL and linger (#468)`.

### Task 11: Verification

- [ ] `go test ./...`, `GOOS=linux go test ./...` (compile-only where hardware-gated), `golangci-lint run ./...`, `GOOS=linux golangci-lint run ./...`, `nix flake check` if nix is available.
- [ ] Update PR body with results; mark ready.
