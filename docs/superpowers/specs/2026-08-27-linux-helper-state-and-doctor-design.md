# Linux helper state ownership and doctor fixes (#467, #468)

## Problem

The packaged Linux `tbx-helper` (systemd `User=tbx`, `CAP_NET_*` only) cannot
serve any cluster:

1. `cmd/tbx-helper/main.go` resolves and validates the socket path before it
   checks for a systemd-activated FD. Under the unit the resolved path is
   `/run/user/<tbx-uid>/tbx-helper.sock`, which never exists → fatal exit →
   start-limit → socket unit down.
2. The helper reads cluster state via `cluster.List()` → `$HOME/.talosbox`.
   As the `tbx` system user that is empty: no DHCP listeners, no nftables
   rules, no bridges reconverged.
3. The unit's capability model forbids reading a user's 0700 home, so (2)
   cannot be fixed by pointing `HOME` anywhere.

Separately (#468), `tbx doctor` on Linux execs macOS binaries
(`/sbin/route`, `/usr/bin/systemextensionsctl`) and every "helper
unreachable" path prints the macOS-only `sudo tbx system install`.

## Design

### State ownership (Linux)

`tbxd` is the source of truth; the helper holds a pushed copy.

- **Protocol v5** adds op `net.sync` with args
  `{"clusters":[{"name":string,"subnetIndex":int,"nodes":[{"mac":string,"ip":string}]}]}`.
  The helper validates the set with `cluster.NewReservationTable`, replaces its
  in-memory `helperState`, persists it, and reconverges (bridges/nftables via
  `convergeLinuxManagedState`, DHCP via `dhcp.Converge`). Response: `{}`.
- **Persistence**: `$STATE_DIRECTORY/reservations.json` when systemd sets
  `STATE_DIRECTORY`; otherwise `$TBX_HELPER_STATE_DIR`; otherwise no
  persistence (in-memory only, logged once). The unit gains
  `StateDirectory=tbx` (→ `/var/lib/tbx`, writable under
  `ProtectSystem=strict`).
- **Startup**: the helper loads the persisted file (missing file = empty set)
  and converges from it before serving, so host networking survives helper
  restarts with no `tbxd` running.
- **Attach**: `net.attach` continues to converge DHCP; the desired subnet set
  is now the union of the synced state and live attachments' subnet indexes,
  so a node can never be attached to a subnet with no DHCP listener.
- **Client**: `(*Client).Sync(clusters []cluster.Cluster) error`. `tbxd` calls
  it from a single `syncHelperState()` in `internal/daemon` on daemon start and
  after every `cluster.Save`/`cluster.Destroy` site. Failure to sync is logged
  and returned on the create/start paths (a cluster cannot boot without DHCP)
  and logged only on destroy/stop.
- macOS: `net.sync` is accepted and stored but has no effect (vmnet owns
  DHCP). Darwin `cluster.List()` use in the helper is unchanged (none).
- `nix/vm-test.nix` protocol literal → 5.

### Startup ordering (defect 1)

`run()` calls `systemd.InheritedListener` first. When activated, the socket
path is never resolved or validated (the FD is the address). Only the
non-activated path resolves and validates.

### Doctor (#468)

1. `checkClusterRoutes` takes a `routeInterface func(ip string) (string, error)`
   seam. Darwin: `/sbin/route -n get` (existing parser). Linux:
   `ip -o route get <ip>` parsed for the `dev` token. Cluster-interface match
   is per-GOOS: darwin `bridge*`/`vmnet*` + `lo0` for the gateway; linux
   `br-tbx*` + `lo` for the gateway.
2. `securityInventoryFindings` is darwin-only (build tag); Linux emits no
   `security-inventory` finding.
3. A single `helper.UnavailableAdvice()` returns
   `linuxHelperReinstallAdvice`-style wording on Linux ("enable and start
   `tbx-helper.socket`; see docs/linux.md") and the `sudo tbx system install`
   wording elsewhere. All three sites (`daemon/operations.go`,
   `cmd/tbxd/networking.go`, `cmd/tbx-helper/privileges_other.go`) use it.
4. KVM group remediation: `linuxSessionRefreshHint(osrelease string)` returns
   "run `wsl --shutdown` from Windows, then reopen the distro" when
   `/proc/sys/kernel/osrelease` contains `microsoft` (case-insensitive),
   otherwise "log out and back in (a lingering user session — `loginctl
   enable-linger` — needs `loginctl terminate-user $USER`)".

### Testing

- Unit tests, hermetic: fake network ops, temp state dirs, injected osrelease.
- `scripts/ci/linux-kvm-storage-e2e.sh`: when `systemctl` is usable, install
  the packaged units and start the helper through `tbx-helper.socket`;
  otherwise keep the current direct launch and log
  `packaged-unit coverage skipped: no systemd`.

### Out of scope

The empty-`table inet tbx` observation in #467 has no repro; startup
reconverge from persisted state is the likely fix and is noted in the PR.
