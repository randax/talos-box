# Support bundles: how other CLIs build, cap, redact and hand over diagnostics

Research for [#568](https://github.com/randax/talos-box/issues/568) (map [#567](https://github.com/randax/talos-box/issues/567)). Date: 2026-09-03.

**Bottom line: almost nobody redacts.** Of the eight-plus tools surveyed, exactly
two have real scrubbing machinery — `sos` (a full consistent-pseudonymisation
subsystem) and `gh` (auth-header masking, and only inside its HTTP tracer). Every
other bundle in wide use — `talosctl support`, `kind export logs`, Docker Desktop
diagnose, `kubectl cluster-info dump` — ships pod logs, journals, process argv and
container environment verbatim, and manages the risk by *encrypting* the archive
(Talos), *restricting access plus a retention window* (Docker), or simply *not
uploading it anywhere* (kind). The `tbx report` PII contract as drafted in #567
would put tbx ahead of every tool here except `sos`.

Second finding: **neither `talosctl support` nor `kind export logs` writes a
manifest**, so a consumer cannot tell a missing collector from an empty one.
`sos` does, and packs it first in the tarball so a triager can read it without
extracting the archive. That is the cheapest good idea in the whole survey.

Sources are upstream source at a pinned ref, or first-party docs, linked inline.
Where a claim is a design reading rather than a quoted line it is labelled.

## Summary table

| | artifact | manifest | caps | redaction | consent | id | upload |
|---|---|---|---|---|---|---|---|
| `talosctl support` | one zip, age-encrypted by default | none | none | COSI `Sensitive` specs → `<REDACTED>`, nothing else | overwrite prompt + recipient list | cluster name in filename only | none |
| `kind export logs` | plain directory in a tempdir | none | none | none at all | none | none | none |
| `tailscale bugreport` | none — a marker id into the always-on log stream | n/a | 256 KiB/entry, 16 KiB/text, 50 MiB spool | none on this path | none (ambient) | `BUG-<pubID>-<ts>-<64b rand>` | continuous logtail POST |
| Docker Desktop diagnose | local zip, upload opt-in | not published | none published | none, disclosed instead | GUI: separate "Upload" click; CLI: `-upload` flag | `<user UUID>/<ts>` | undocumented; 30-day retention |
| `sos report` / `sos clean` | tar.xz + a separate private map file | `sos_reports/manifest.json`, packed first | `--log-size`, default 25 MiB | two layers: plugin `postproc()` regexes + the cleaner's consistent pseudonymisation | banner + `Press ENTER … CTRL-C to quit`, `--batch` | archive name | manual |
| `gh` | no bundle | — | 100 KB logged body | `Authorization`/`Cookie` → fixed-width `████`; `maskToken` prefix-preserving | — | — | — |
| `limactl`, `k9s`, `minikube`, `colima`, `crc`, `vagrant` | no bundle command | — | — | none | — | — | — |

---

## 1. `talosctl support` (siderolabs/talos)

Pinned at talos **v1.14.0** and `github.com/siderolabs/go-talos-support` **v0.3.1**
(the CLI is a thin shell; all collection lives in the support module).

- [`cmd/talosctl/cmd/talos/support.go`](https://github.com/siderolabs/talos/blob/v1.14.0/cmd/talosctl/cmd/talos/support.go)
- [`support/support.go`](https://github.com/siderolabs/go-talos-support/blob/v0.3.1/support/support.go),
  `support/bundle/{bundle,options}.go`, `support/collectors/{collectors,talos,kubernetes}.go`,
  `support/encryption/`
- Layout is asserted by [`internal/integration/cli/support.go`](https://github.com/siderolabs/talos/blob/v1.14.0/internal/integration/cli/support.go) —
  the test hard-codes an expected file list precisely *because nothing in the artifact declares one*.

### Format and name

A streamed `archive/zip`, optionally wrapped in an [age](https://age-encryption.org) layer.

```go
supportCmdFlags.output = "support"
if info, err := getClusterInfo(ctx, clientFactory); err == nil && info.TypedSpec().ClusterName != "" {
    supportCmdFlags.output += "-" + info.TypedSpec().ClusterName
}
supportCmdFlags.output += ".zip"
if !supportCmdFlags.noEncryption {
    supportCmdFlags.output += ".age"
}
```

Default `support-<cluster>.zip.age`. **No timestamp and no run id** anywhere — not
in the name, not inside the archive.

### Layout

One top-level directory per node, keyed by the `--nodes` string; cluster-level
entries sit at the archive root with no prefix.

```
<node>/dmesg.log
<node>/controller-runtime.log
<node>/dns-resolve-cache.log
<node>/dependencies.dot                       # COSI controller graph, graphviz
<node>/mounts  <node>/devices  <node>/io  <node>/processes
<node>/summary                                # client + server talos version
<node>/resources/<type>.yaml                  # one per COSI ResourceDefinition
<node>/service-logs/<svc>.log
<node>/service-logs/<svc>.state
<node>/kubernetes-logs/kube-system/<pod>[-exited].log
kubernetesResources/nodes.yaml
kubernetesResources/systemPods.yaml
```

A collector returning `nil` writes no entry at all (`Collector.Run`:
`if data == nil { return nil }`), so an empty resource type is silently absent —
indistinguishable from a failed one.

### Caps

**None.** `tailLines` is hard-coded `-1` for every log collector, dmesg is read to
EOF, and each collector buffers its whole payload in memory before one
`archive.Write(path, contents)`. The only knobs are `-w/--num-workers` and `--nodes`.

### Redaction — exactly one mechanism

`collectors/talos.go`:

```go
data := struct {
    Metadata *resource.Metadata `yaml:"metadata"`
    Spec     any                `yaml:"spec"`
}{Metadata: r.Metadata(), Spec: "<REDACTED>"}

if rd.TypedSpec().Sensitivity != meta.Sensitive {
    data.Spec = r.Spec()
}
```

`grep -riE "redact|sanitiz|scrub|mask"` over `go-talos-support/support/` returns
**that one line and nothing else**. So:

- **Machine config secrets are redacted** — `MachineConfig`, `cluster.Config`, the
  whole `secrets.*` family, `k8s.APIServerConfig`, `KubeletConfig`,
  `EtcdEncryptionConfig`, and `network.LinkSpec`/`OperatorSpec` (wifi/PPP creds)
  are all `Sensitivity: meta.Sensitive`, so their `resources/*.yaml` carries
  metadata and `spec: <REDACTED>`.
- **Nothing else is.** `processes` prints full argv including flags,
  `kubernetesResources/systemPods.yaml` is a raw `PodList` with env vars and
  container args, and `service-logs/*.log` plus `kubernetes-logs/**` contain
  whatever the components printed — join tokens and bearer tokens included.
- kubeconfig is *fetched* off the node (`c.Kubeconfig(ctx)`) to build the k8s
  client but is **not written into the bundle**.

The real mitigation is **encryption, not redaction**: by default the zip is
age-encrypted to the public SSH keys of public `siderolabs` org members, generated
by `default_recipients.go` from `https://github.com/<login>.keys` and embedded as
`recipients.txt`. That is a deliberate statement — *the bundle contains things you
should not hand around, so only the vendor can open it* — and it is a much cheaper
engineering answer than scrubbing. Its cost: the user cannot read their own bundle
without `--no-encryption` or adding themselves via `--encryption-recipients`.

### Errors, consent, upload

A failing collector never aborts the bundle **and is never recorded inside it**;
errors go to a stderr table after the archive closes:

```
Processed with errors:
        SOURCE          ERROR
        10.5.0.2        error reading from stream: ...
Support bundle is written to support-mycluster.zip.age
```

Node-supplied strings pass through `safeout.Cell` (terminal-escape defence) before
printing — worth stealing independently.

Consent UX is one overwrite guard, `"%s already exists, overwrite? [y/N]: "`, plus
a printed recipient list when encrypting. **No content preview, no warning about
what is inside.** No upload path, no telemetry, no endpoint: the user attaches the
file manually.

---

## 2. `kind export logs` (kubernetes-sigs/kind)

Pinned at **v0.33.0**.
[`pkg/cmd/kind/export/logs/logs.go`](https://github.com/kubernetes-sigs/kind/blob/v0.33.0/pkg/cmd/kind/export/logs/logs.go),
[`pkg/cluster/provider.go#L238`](https://github.com/kubernetes-sigs/kind/blob/v0.33.0/pkg/cluster/provider.go#L238),
`pkg/cluster/internal/logs/logs.go`.

Not an archive at all — a **plain directory tree**, by default an anonymous OS
tempdir whose path is printed on **stdout** (deliberately machine-parseable, since
the real consumer is CI), while the human message goes to stderr:

```
Exporting logs for cluster "kind" to:
/tmp/396758314
```

```
kind-version.txt
docker-info.txt                      # or podman-info.txt
<node-container>/serial.log          # docker logs <node>
<node-container>/inspect.json        # docker inspect <node>
<node-container>/kubernetes-version.txt
<node-container>/journal.log  kubelet.log  containerd.log   # journalctl --no-pager
<node-container>/images.log          # crictl images
<node-container>/<everything under the node's /var/log/>
```

The last line is `DumpDir(logger, n, "/var/log", dir)`, which tars `/var/log` off
the node with `tar --hard-dereference -C /var/log/ -chf - .` and untars it
*directly into the node directory*. `-h` dereferences the `/var/log/containers/*.log`
symlinks, so pod logs are materialised as full copies.

Everything is an `exec` whose stdout **and stderr** both go to the destination file,
so a failed command leaves its error text in-band, in the log it was meant to
produce — a poor man's error record, but a real one. Both levels use
`errors.AggregateConcurrent`, so a failure never aborts the run; the command exits
non-zero but the directory still holds everything that succeeded.

Only `nodeutils.InternalNodes` (role `worker` or `control-plane`) are visited, so
**`kind-external-load-balancer` logs are never captured** — an easy gap to miss.
There are **no Kubernetes API calls at all**.

**Caps: none.** `journalctl --no-pager` with no `-n`/`--since`, `docker logs` with
no `--tail`, `/var/log` copied wholesale.

**Redaction: zero.** `grep -riE "redact|sanitiz|scrub|mask"` over `pkg/cluster/`
and `pkg/cmd/` finds only `sanitizeImage` (a name normaliser) and `net.CIDRMask`.
Concretely, `inspect.json` is raw `docker inspect` — all container env, mounts and
labels verbatim — and `/var/log/pods/**` is the complete stdout/stderr of every pod
on the node. `journal.log` routinely carries the kubeadm bootstrap token.

**Consent: none whatsoever.** No prompt, no overwrite guard (existing files are
clobbered via `os.Create`), no summary, no warning. **Identification: none** —
cluster name appears only in the stderr message; successive runs into the same
explicit directory silently merge. **Upload: none**; the docs frame it as a
self-service aid and CI artefact.

Defensible for throwaway dev clusters, but it is an explicit non-feature, not an
oversight someone has quietly handled.

---

## 3. `tailscale bugreport`

Pinned at `tailscale/tailscale@6c06e00ad3b72b6522be30c0fdbb5285a0376935`.

**There is no local bundle.** The command prints one opaque marker; the payload is
written into the node's ordinary `logtail` stream, which is already uploading
continuously to `log.tailscale.com`. The marker is a *bookmark into that stream*.
The docs say so outright: a bug report is "a random indicator that marks a section
of the diagnostic logs" ([kb/1227](https://tailscale.com/kb/1227/bug-report)).

Flow: `cmd/tailscale/cli/bugreport.go` → `POST /localapi/v0/bugreport` →
[`(*Handler).serveBugReport`](https://github.com/tailscale/tailscale/blob/6c06e00ad3b72b6522be30c0fdbb5285a0376935/ipn/localapi/localapi.go#L395),
which `h.logf(...)`s everything and then
`defer h.b.TryFlushLogs() // kick off upload after bugreport's done logging`.

### Report id

```go
logMarker := func() string {
    return fmt.Sprintf("BUG-%v-%v-%v", h.backendLogID,
        h.clock.Now().UTC().Format(tstime.NumericDateTimeZ), rands.HexString(16))
}
if envknob.NoLogsNoSupport() {
    logMarker = func() string { return "BUG-NO-LOGS-NO-SUPPORT-this-node-has-had-its-logging-disabled" }
}
```

`BUG-<logtail PublicID>-<YYYYMMDDhhmmssZ07:00>-<16 hex>`. The `PublicID` is the
**SHA-256 of the node's logtail PrivateID** — a pseudonym, not a raw identity, and
already known to the vendor. The 64 random bits make the marker greppable in the
stream. Support looks up `(instance public ID, timestamp)` and greps the suffix.

### What is logged

Each as a `user bugreport …` line: the free-text note verbatim; full JSON of
`hostinfo.New()` (hostname's first DNS label, OS + version, distro, container flag,
package type, GoArch, uname `Machine`, DeviceModel, cloud provider, version);
health status; `nodeid`/`stableid`/`expiry`; **machine and node public keys**; every
set `TS_*` env knob; `osdiag.SupportInfo` (on Windows: registry values, installed
AV/security software, services); tailnet lock status.

`--diagnose` adds `diag: `-prefixed lines from the doctor hook — uid/gid/capability
sets, the full route table, ethtool, DNS resolvers — rate-limited because it can
outrun logtail:

```go
// We can write logs too fast for logtail to handle, even when
// opting-out of rate limits. Limit ourselves to at most one message
// per 20ms and a burst of 60 log lines...
logf = logger.SlowLoggerWithClock(ctx, logf, 20*time.Millisecond, 60, b.Clock().Now)
```

`--record` writes a start marker, then **turns magicsock component debug logging on
for 12 hours**, blocks on Enter, and writes an end marker — it materially increases
what is uploaded.

### Caps

[`logtail/logtail.go`](https://github.com/tailscale/tailscale/blob/6c06e00ad3b72b6522be30c0fdbb5285a0376935/logtail/logtail.go#L46-L60):

```go
const maxSize     = 256 << 10  // max single entry and max upload body
const maxTextSize = 16 << 10   // max text log message (JSON may be maxSize)
const lowMemRatio = 4
const maxUploadTime = 45 * time.Second
```

Over-length text is truncated with a `…+<N>` suffix naming the dropped byte count.
Over-length JSON becomes an error entry `"entry too large: %d bytes"` carrying a
truncated copy. On-disk spool cap `DefaultMaxFileSize = 50 << 20`.

### Redaction — none on this path

Grepping `redact|scrub|sanitiz|omitPII` finds nothing in `serveBugReport`, the
doctor checks, or `logtail`. Public keys, node ids, hostname, uid/gid/caps and the
full route table go in cleartext. What exists is adjacent and instructive:

- [`util/goroutines.ScrubbedGoroutineDump`](https://github.com/tailscale/tailscale/blob/6c06e00ad3b72b6522be30c0fdbb5285a0376935/util/goroutines/goroutines.go#L14)
  — "with the actual values of arguments scrubbed out, lest it contain some private
  key material." Not a regex: every hex address `0x…` becomes a stable synthetic
  token `v<N>%<addr mod 8>`, **preserving aliasing and alignment while destroying
  the value**. Used by `c2n`, not bugreport.
- `ipn/ipnlocal/c2n.go` `redactAndMarshal` strips private keys from netmap dumps.
- `cmd/tailscale/cli/cli.go` `sanitizeWriter` overwrites characters after known
  key-ish prefixes with `'X'` when *flag-parse errors* print. Prefix scan, not regex.
- `ipn/backend.go`: `NotifyNoPrivateKeys … // (no-op) it used to redact private keys; now they always are`.

The privacy story is not redaction: it is that the payload goes to a first-party
store the user already opted into, and only the *marker* is handed to a human.

### Consent

**No preview, no prompt.** Output is the bare marker. Help text is
`"Print a shareable identifier to help diagnose issues"`. Only `--record` is
interactive:

```
Recording started; please reproduce your issue and then press Enter...
Please provide both bugreport markers above to the support team or GitHub issue.
```

Consent is inverted relative to a bundle tool: consent to *collection* was given
once by leaving logging on; the per-incident act is consenting to *disclosure* by
pasting the marker. Docs claim reviewability — "you can review exactly what was
sent by reading those logs" — backed by `tailscale debug daemon-logs`, which taps
the same JSON blobs locally (`RegisterLogTap`/`tapSend`). Opt-out is global:
`--no-logs-no-support`, which prints *"You have disabled logging. Tailscale will
not be able to provide support."*

### Upload contract

`POST https://log.tailscale.com/c/<collection>/<PrivateID hex>`. **The 32-byte
PrivateID is the write credential, in the URL path.** Headers:
`Content-Encoding: zstd` + `Orig-Content-Length` when compressed;
`req.Header["User-Agent"] = nil // not worth writing one; save some bytes`; no
Authorization header; TLS 1.3 forced. Body is newline-framed JSON. Honours
`Retry-After`. Retention: unadopted public IDs are "tightly capped and logs are
deleted after 12 hours"; adopted instances get the configured retention.

---

## 4. Docker Desktop `docker desktop diagnose` / `com.docker.diagnose`

Closed source; from [docs.docker.com](https://docs.docker.com/desktop/troubleshoot-and-support/troubleshoot/),
the [CLI reference](https://docs.docker.com/reference/cli/docker/desktop/diagnose/), and the
[archived troubleshooting page](https://github.com/docker/docker.github.io-1/blob/master/desktop/mac/troubleshoot.md)
(materially more explicit about privacy).

Opposite design to Tailscale: **a local ZIP always, an id only if you upload.**

- macOS/Linux: `/tmp/<diagnostics-ID>.zip`
- Windows: `%LOCALAPPDATA%\Temp\<user-uuid>\<timestamp>.zip`, e.g.
  `C:\Users\testUser\AppData\Local\Temp\5DE9978A-…-950FC869186F\20230607101602.zip`
  — the directory *is* the user UUID and the filename *is* the timestamp.

```console
$ docker desktop diagnose            # -u, --upload : "Uploads the diagnostic ID."
# or directly:
/Applications/Docker.app/Contents/MacOS/com.docker.diagnose gather -upload
```

`gather` without `-upload` produces the local ZIP only — that flag is the consent seam.
The old `com.docker.diagnose check` self-diagnose (PASS/FAIL per check) is deprecated:
`The 'check' command is deprecated. Please use 'gather' to generate a diagnostics bundle.`
Its check IDs (`DD0012` etc.) survive as user-visible failure codes.

**Contents are not enumerated by Docker.** This is a documentation gap worth citing
as a negative example: the only positive characterisation is the privacy warning.
No caps are documented; the only quantitative statement is "Gathering diagnostics
may take several minutes."

**Redaction: explicitly none, disclosed rather than mitigated:**

> "the uploaded diagnostics bundle may contain personal data such as usernames and IP addresses."
> "Diagnostics bundles are only accessible to Docker, Inc. employees directly involved in diagnosing issues."
> "Docker, Inc. will delete uploaded diagnostics bundles after 30 days."

**Id:** "composed of your user ID and a timestamp. For example
`BE9AFAAF-F68B-41D0-9D12-84760E6B8740/20190905152051`" — a **stable per-user UUID**
plus a per-run timestamp, correlatable across every bundle that user ever files.
Contrast Tailscale, where the stable half is a hash-derived pseudonym. Docs warn to
"always provide the complete ID, not just the user portion." Unsubscribed users are
routed to a public GitHub issue, so that stable UUID often ends up posted publicly.

**Consent (GUI)** is three steps with upload as a distinct affirmative click:
"select **Get support**" → "select **Upload to get a Diagnostic ID**" → "Copy this
ID." The error-triggered path ("select **Gather diagnostics**") is weaker — the
archived wording notes results upload automatically. **CLI consent is the flag
itself**: no preview, no confirmation, no summary. The docs' suggested review
(`unzip -l /tmp/<id>.zip`) is offered *post hoc*, not as a pre-upload gate.

**Upload contract:** no endpoint, headers, auth scheme or content type is published.
Only the retention (30 days, deletion on request by ID) and the access limitation.

---

## 5. `sos report` / `sos clean` — the only complete prior art

`sosreport/sos` @ `88ef17cb7ee07b9e5c0a6116315e234888e2a6a3`. This is the one design
in the survey that has actually solved the problem, and most of what tbx should
borrow comes from here.

### Layout and manifest

```python
self.cmddir = 'sos_commands'
self.logdir = 'sos_logs'
self.rptdir = 'sos_reports'
```

| Path | Content |
|---|---|
| mirror of `/` | copied config/log files at their real absolute paths |
| `sos_commands/<plugin>/<mangled cmd>` | one file per executed command |
| `sos_logs/sos.log`, `ui.log`, `cleaner.log` | sos's own run logs |
| `sos_reports/manifest.json` | machine-readable run manifest |
| `version.txt` | `sos report: <version>` |
| `checksums/<arc>.sha256` | per-archive checksums (collect mode) |

**Metadata is packed first**, so a triager can stream-read it
([`sos/archive.py#L790-L798`](https://github.com/sosreport/sos/blob/88ef17cb7ee07b9e5c0a6116315e234888e2a6a3/sos/archive.py#L790-L798)):

```python
# Add commonly reviewed files first, so that they can be more
# easily read from memory without needing to extract
# the whole archive
for _content in ['version.txt', 'sos_reports', 'sos_logs']:
```

Command→filename mangling
([`plugins/__init__.py#L49`](https://github.com/sosreport/sos/blob/88ef17cb7ee07b9e5c0a6116315e234888e2a6a3/sos/report/plugins/__init__.py#L49-L54)),
directly adaptable:

```python
def _mangle_command(command, name_max):
    mangledname = re.sub(r"^/(usr/|)(bin|sbin)/", "", command)
    mangledname = re.sub(r"[^\w\-\.\/]+", "_", mangledname)
    mangledname = re.sub(r"/", ".", mangledname).strip(" ._-")
    return mangledname[0:name_max]
```

Manifest fields: `version`, `cmdline`, `start_time`, `end_time`, `run_time`,
`compression`, `policy`; `components.report` with `enabled_plugins`,
`disabled_plugins`, and per-plugin `setup_time`/`run_time`/`postproc_*`. The cleaner
appends `components.clean.parsers.<parser>.entries` — **a count of obfuscated items
per parser**, a transparency signal that leaks nothing.

### Caps

`--log-size`, default **25 MiB**, "limit the size of collected logs (not journals)
in MiB"; journals have a separate `journal_size`. `--estimate-only` does a dry run
to report the disk requirement — and notably force-disables `--clean` in that mode.

### Redaction layer 1 — plugin `postproc()`, known-key regexes

Before the cleaner runs, each plugin scrubs its own secrets. Uniform style: **keep
the key, replace the value with asterisks** (`\1*********`).

```python
_certmatch = re.compile("----(?:-| )BEGIN.*?----(?:-| )END", re.DOTALL)
_cert_replace = "-----SCRUBBED"
```

The docstring for `do_file_private_sub` states the intent well: matches become
`"-----SCRUBBED $desc"`, e.g. `-----SCRUBBED RSA PRIVATE KEY`, "so that support
representatives can at least be informed of what type of content it was originally."

The key-list → regex idiom, repeated across plugins:

```python
protect_keys = ["auth.password", "auth.token", "tls.key_pass"]
# Redact yaml and ini style "key (:|=) value".
keys_regex = fr"(^\s*({'|'.join(protect_keys)})\s*(:|=)\s*)(.*)"
sub_regex = r"\1*********"
```

URL credentials, keeping the user and masking the password:

```python
# scheme://user:PASS@host
self.apply_regex_sub(fr"(^\s*({join_con_keys})\s*=\s*(.*)://(\w*):)(.*)(@(.*))",
                     r"\1*********\6")
http_proxy_regexp = r"(http(s)?://)\S+:\S+(@.*)"
http_proxy_repl   = r"\1******:******\3"
```

### Redaction layer 2 — the cleaner's consistent pseudonymisation

`sos/cleaner/`, run as `sos clean|mask TARGET` or inline as `--clean`/`--mask`.
Six parsers, each with a mapping store. **Every mapping is deterministic** — the
same input always yields the same output, within *and across* runs — so topology
and correlation survive obfuscation.

**Hostname** → `host{N}` / `obfuscateddomain{N}`, TLD preserved:

```python
regex_pattern = re.compile(r'(((\b|_)[a-zA-Z0-9-\.]{1,200}\.[a-zA-Z]{1,63}(\b|_)))')
ignore_matches = ['localhost', '.*localdomain.*', '^com..*']
skip_keys = ['www', 'api']
strip_exts = ('.yaml', '.yml', '.crt', '.key', '.pem', '.log', '.repo', '.rules', '.conf', '.cfg')
```

`strip_exts` is how it obfuscates *filenames* containing hostnames: strip the
extension, obfuscate the stem, reattach. Dots are matched as `(?:\.|_)` so
`web1_corp_example_com` in a filename matches the FQDN. Word boundaries via
`rf'(?<![a-z0-9])(?:{item})(?![a-z0-9])'`. Names ≤2 chars become the literal
`unknown` rather than minting a mapping.

**IPv4** → sequential networks from `100.0.0.0`, singletons from `172.17.0.0`:

```python
regex_pattern = re.compile(r'((?<!(-|\.|\d))([0-9]{1,3}\.){3}([0-9]){1,3}(\/([0-9]{1,2}))?)')
skip_line_patterns = [r'.*dnf\[.*\]:']
parser_skip_files = [
    'installed-debs', 'installed-rpms', 'sos_commands/dpkg',
    'sos_commands/python/pip_list', 'sos_commands/rpm',
    'sos_commands/yum/.*list.*', 'sos_commands/snappy/snap_list_--all',
    'sos_commands/vulkan/vulkaninfo', 'etc/rhsm/facts/satellite.facts',
    'var/log/.*dnf.*', 'var/log/.*packag.*',
    '.*(version|release)(\\.txt)?$',
]
ignore_matches = [r'127.*', r'::1', r'0\.(.*)?', r'1\.(.*)?',
                  r'8.8.8.8', r'8.8.4.4', r'169.254.*', r'255.*']
network_first_octet = 100
skip_network_octets = ['127', '169', '172', '192']
_saddr_cnt = 2886795264   # == 172.17.0.0
```

`parser_skip_files` is the single most useful practical lesson in the survey:
**version strings look exactly like IPv4 addresses**, and a naive scrubber mangles
every package list and version file it touches. Prefix lengths are preserved
(`192.168.1.0/24` → `100.0.0.0/24`, with `.1`/`.2` mapping inside it), and
loopback, link-local, broadcast and public DNS are deliberately left intact
because they carry diagnostic meaning and no identity.

**IPv6** → `534f:` (global) / `fd53:` (ULA), i.e. "SOS" in hex, chosen so an
obfuscated address can never collide with a real one:

```python
_quick_check = re.compile(r'::|[0-9a-f]{1,4}:[0-9a-f]{1,4}:', re.I)
regex_pattern = re.compile(
    r"(?<![:\\.\\-a-zA-Z0-9])"
    r"((([0-9a-fA-F]{1,4})(:[0-9a-fA-F]{1,4}){7})|"
    r"(([0-9a-fA-F]{1,4}(:[0-9a-fA-F]{0,4}){0,5}))"
    r"([^.])::(([0-9a-fA-F]{1,4}(:[0-9a-fA-F]{1,4}){0,5})?)(\/\d{1,3})?)"
    r"(?!([a-zA-Z0-9]|:[a-zA-Z0-9]))"
)
ignore_matches = [r'^::1/.*', r'::/0', r'fd53:.*', r'^53..:']
```

The lookbehind stops `SomeFunc::ADiffFunc` in a C++ log line reading as an address.
`fe80::/64` link-local is deliberately kept — "retaining the information that an
address is a link-local address is important for problem analysis". The
`_quick_check` cheap pre-filter in front of an expensive regex is worth copying
wholesale for per-line scanning.

**MAC** → `53:4f:53:xx:xx:xx` (the OUI is ASCII "SOS"):

```python
IPV6_REG_8HEX = (r'((?<!([0-9a-fA-F\'\"]:)|::)(?:[^:|-])?(?:[0-9a-fA-F]{2}(?:(:|-)){7})'
                 r'[0-9a-fA-F]{2}(?:\'|\")?(?:\/|\,|\-|\.|\s|$))')
IPV4_REG      = (r'((?<!([0-9a-fA-F\'\"]:)|::)'
                 r'(?:([^:\-])?(?:([0-9a-fA-F]{2}([:\-\_])){5,6}(?:[0-9a-fA-F]{2}))))')
_quick_check  = re.compile(r'[0-9a-fA-F]{2}[:\-_][0-9a-fA-F]{2}')
obfuscated_patterns = ('53:4f:53', '534f:53')   # avoid double scrubbing
mac_template  = '53:4f:53:%s:%s:%s'
ignore_matches = ['ff:ff:ff:ff:ff:ff', '00:00:00:00:00:00']
```

Input is normalised (dashes → colons, lowercased, `=.,` stripped) before lookup.

**Username** → `obfuscateduser{N}`, and crucially **no regex at all**:

> Note that this parser does not rely on regex matching directly, like most other
> parsers do. Instead, usernames are discovered via scraping the collected output
> of lastlog. As such, we do not discover new usernames later on, and only
> usernames present in lastlog output will be obfuscated, and those passed via the
> `--usernames` option if one is provided.

`ignore_short_items = True`, `match_full_words_only = True`, set-based token lookup.
**This "harvest an authoritative list first, then substitute known values" approach
is far safer than regexing free text** — and it is exactly the shape of the
known-values-first scrubber already decided for tbx in #567.

**Keyword** → `obfuscatedword{N}`, user-supplied via `--keywords`/`--keyword-file`.

### The mapping file (the decode ring)

`compile_mapping_dict` builds `{hostname_map, ip_map, ipv6_map, mac_map,
username_map, keyword_map}` and writes it **next to but not inside** the archive as
`<arcname>-private_map` (the filename itself run through `obfuscate_string`), plus
`/etc/sos/cleaner/default_mapping` — which is what makes obfuscation consistent
across runs. The man page: *"a mapping file … should be kept private by system
administrators."*

### File-level policy

Some content cannot be line-scanned, so it is dropped outright:

```python
obvious_removes = [r'.*\.gz$', r'.*\.xz$', r'.*\.bzip2$', r'.*\.tar\..*',
                   r'.*\.txz$', r'.*\.tgz$', r'.*\.bin$', r'.*\.journal$', r'.*\~$']
```

`--keep-binary-files` opts out with an explicit warning. `--treat-certificates
{obfuscate|keep|remove}` defaults to `obfuscate` (convert to text, then scrub), and
**private key files are always removed regardless of the setting**.

### Consent and preview wording — verbatim

Pre-run banner (`sos/policies/__init__.py`, wrapped to 72 columns):

```
This command will collect system configuration and diagnostic information
from this %(os_release_name)s system.

For more information on %(vendor)s visit:

  %(vendor_urls)s

The generated archive may contain data considered sensitive and its content
should be reviewed by the originating organization before being passed to
any third party.

%(changes_text)s
```

where `changes_text` is literally one of:

```
Changes CAN be made to system configuration.
No changes will be made to system configuration.
```

then, unless `--batch`:

```python
msg += _("Press ENTER to continue, or CTRL-C to quit.\n")
input(msg)   # KeyboardInterrupt -> "Exiting on user cancel", exit 130
```

The `sos clean` disclaimer is the more interesting one, because it manages
expectations about coverage instead of overclaiming:

```
This command will attempt to obfuscate information that is generally
considered to be potentially sensitive. Such information includes IP
addresses, MAC addresses, domain names, and any user-provided keywords.

Note that this utility provides a best-effort approach to data obfuscation,
but it does not guarantee that such obfuscation provides complete coverage of
all such data in the archive, or that any obfuscation is provided to data that
does not fit the description above.

Users should review any resulting data and/or archives generated or processed
by this utility for remaining sensitive content before being passed to a
third party.

Press ENTER to continue, or CTRL-C to quit.
```

Post-run:

```
Successfully obfuscated N report(s)

A mapping of obfuscated elements is available at
	/var/tmp/<name>-private_map

The obfuscated archive is available at
	/var/tmp/<obfuscated-name>.tar.xz

	Size	12.34MiB
	Owner	root

Please send the obfuscated archive to your support
representative and keep the mapping file private.
```

Other flags worth noting: `--disable-parsers`, `--skip-cleaning-files`,
`--domains`, `--usernames`, `-j/--jobs` (default 4), `--archive-type`.

---

## 6. `gh` (cli/cli) — how to mask an auth header

`cli/cli` @ `636cbb6ddb93c1f63f3ba01c74f94705787561f1`, `cli/go-gh` v2.15.0,
`henvic/httpretty` v0.2.0. **No bundle command**; the relevance is the redaction
machinery, which lives one and two layers down the dependency tree.

`GH_DEBUG` wires an `httpretty.Logger` into the transport (`go-gh
pkg/api/http_client.go`), with two secondary controls worth noting:
`MaxResponseBody: 100000` (a hard 100 KB truncation on logged bodies) and a
**content-type allowlist** rather than a denylist — only `text/*`,
`application/x-www-form-urlencoded` and JSON bodies are ever dumped.

Sanitisation happens at *print* time, so transport ordering cannot bypass it
([`httpretty/printer.go#L741`](https://github.com/henvic/httpretty/blob/v0.2.0/printer.go#L741-L744)):

```go
func (p *printer) printHeaders(prefix rune, h http.Header) {
	if !p.logger.SkipSanitize {
		h = header.Sanitize(header.DefaultSanitizers, h)
	}
```

```go
var DefaultSanitizers = map[string]SanitizeHeaderFunc{
	"Authorization":       AuthorizationSanitizer,
	"Set-Cookie":          SetCookieSanitizer,
	"Cookie":              CookieSanitizer,
	"Proxy-Authorization": AuthorizationSanitizer,
}

func redact(count int) string {
	if count == 0 {
		return ""
	}
	return "████████████████████"
}
```

`count` is accepted and deliberately **ignored**: the redaction is fixed-width, so
the output leaks nothing about token length. The auth *scheme* is preserved —
`Bearer ████████████████████` — keeping the trace diagnostically useful. Lookup is
via `http.CanonicalHeaderKey`, so casing does not matter. `SkipSanitize` is the
documented opt-out; `gh` never sets it.

`gh`'s own token masking takes the opposite trade
([`pkg/cmd/auth/status/status.go#L332`](https://github.com/cli/cli/blob/636cbb6ddb93c1f63f3ba01c74f94705787561f1/pkg/cmd/auth/status/status.go#L332-L348)):

```go
var knownTokenPrefixes = []string{"github_pat_", "ghp_", "gho_", "ghu_", "ghs_", "ghr_"}

func maskToken(token string) string {
	for _, prefix := range knownTokenPrefixes {
		if strings.HasPrefix(token, prefix) {
			return prefix + strings.Repeat("*", len(token)-len(prefix))
		}
	}
	return strings.Repeat("*", len(token))
}
```

Prefix- and length-preserving, because in `gh auth status` the token *type* is the
diagnostically relevant fact. Unmasking is opt-in and explicit (`--show-token`).

Also relevant to any bundle: `gh` runs every API response through an
`asciisanitizer.Sanitizer` to strip ANSI escapes from server-controlled content
before it reaches a terminal — a separate hazard class from secrets, and equally
live when concatenating untrusted log content into a file a human will `cat`.

---

## 7. The rest: no bundle at all

**`limactl`** (lima-vm/lima @ `659e69a5`) — no bundle/support/diagnostics command.
`limactl debug` is a hidden group whose only subcommand is a live test DNS server
(`Long: "DO NOT USE! THE COMMAND SYNTAX IS SUBJECT TO CHANGE!"`); `limactl info`
prints host env + driver capabilities as JSON and collects nothing. The per-instance
file inventory is in `pkg/limatype/filenames/filenames.go`: `ha.stdout.log`
(hostagent stdout, **JSON lines**), `ha.stderr.log`, `driver.stderr.log`,
`serial.log`, `serialp.log`, `serialv.log`, `lima.yaml`, `lima-version`, `ha.pid`,
`ssh.config`, `cloud-config.yaml`, `ansible-inventory.yaml` — and, in the same tree,
**`VNCPasswordFile` and `UserPrivateKey` in plaintext**, so a naive "tar the
instance dir" bundle ships a private key and a VNC password. Redaction: none. The
FAQ's debugging ask is telling:

```
- Inspect logs:
    - `limactl --debug start`
    - `$HOME/.lima/<INSTANCE>/serial.log`
    - `/var/log/cloud-init-output.log` (inside the guest)
    - `/var/log/cloud-init.log` (inside the guest)
```

Two of the four are *inside the guest* — a bundle for a VM CLI has to reach into
the guest, not just scrape the host-side instance dir.

**`k9s`** (@ `84852e6e`) — no dump/bundle command; `k9s info` prints *locations
only* and reads nothing. Logs at `<XDG>/k9s.log`. The issue template asks for "k9s
debug logs, configurations, resource manifests" plus OS/K9s/K8s versions. Redaction:
none, and arguably the reverse — `internal/dao/secret.go` implements `Secret.Decode`
/ `ExtractSecrets`, a deliberate reveal feature.

**`minikube`** (@ `4d8e24be`) — no bundle; `minikube logs` has `--file`, `-n/--lines`,
`--follow`, `--audit`, `--last-start-only`, and `--problems`, which filters output
to known-issue lines via `logs.FindProblems` capped at `numberOfProblems = 10`.
That triage filter is worth borrowing independently of bundling. No redaction.

**`kubectl cluster-info dump`** — the only other true multi-file collector: cluster
resource state plus all pod logs, to stdout or a per-file tree under
`--output-directory`, scoped to current + `kube-system` unless `--all-namespaces`.
**No redaction** — it will dump Secret objects.

**`crc`/OpenShift Local** (@ `62da49a1`) — `crc bundle` is a false friend: it builds
a custom OpenShift *disk-image* bundle, not a support bundle. **`colima`**
(@ `c3a5f918`) — no bundle; wraps lima, so its VM logs *are* lima's, under
`~/.colima/<profile>/`, plus its own `daemon.log`. **`vagrant`** (@ `dc55920a`) — no
support command; debug logging is env-var only (`VAGRANT_LOG`, `VAGRANT_LOG_FILE`).

---

## What tbx should borrow

The #567 standing decisions already land close to the best of this survey. What
follows is the concrete shape, and the places the survey says to go further.

### Layout

Tar.gz (already decided), and put a manifest in it — **nobody else does, and both
Talos and kind are worse for it**. Pack metadata first (sos's `archive.py` comment)
so a triager can `tar -xOzf report.tar.gz manifest.json` without extracting the
console buffers.

```
manifest.json                 # first entry in the tar
version.txt                   # tbx + tbxd + helper versions, protocol version
doctor.txt                    # full tbx doctor output, scrubbed
config/talosbox.yaml          # the file in use, scrubbed
message.txt                   # optional --message, scrubbed (omit if absent)
failure.txt                   # failing command line + stderr, when reached via the hint
daemon-logs/tbxd.log          # capped tail
daemon-logs/tbxd.k8s.log      # capped tail
clusters/<pseudonym>/state.json
clusters/<pseudonym>/nodes/<pseudonym>/console.log   # ring buffer, capped
```

Manifest contents, following sos's `manifest.json` and the two transparency ideas
worth stealing from it:

- `reportId`, `createdAt`, `tbxVersion`, `protocolVersion`, `platform`, `hypervisor`
- `collectors`: one entry per collector with `status` (`ok`/`failed`/`skipped`),
  `reason`, `bytes`, and `truncated` — **kind and Talos both drop this on the floor,
  and a consumer then cannot tell a missing collector from an empty one**
- `caps`: the per-file byte/line caps actually applied, so the preview and the
  archive agree
- `scrubber`: per-rule substitution counts, sos's
  `components.clean.parsers.<parser>.entries`. It proves the scrubber ran and how
  much it touched, and leaks nothing.
- `pseudonyms`: the *count* of cluster/node/domain pseudonyms minted, never the map.

**Do not ship a decode ring.** sos writes `<arcname>-private_map` beside the archive
because a sysadmin may legitimately need to de-obfuscate their own bundle. tbx's
attendee case is the opposite — the person filing the report is not the person
debugging it, and there is no support relationship to carry a private map. Keep the
mapping in memory for the run and let it die; stable-within-a-report pseudonyms are
enough to follow topology.

**One deviation from Talos worth making explicitly:** Talos names the archive
`support-<clustername>.zip.age` — the cluster name in the filename defeats the whole
pseudonymisation exercise the moment the file is attached anywhere. tbx's
`~/.talosbox/reports/<report-id>.tar.gz` is already right; keep the cluster name out
of the filename entirely.

### Report id

Both id designs in the survey are instructive by contrast. Docker's
`<stable user UUID>/<timestamp>` is correlatable across every bundle a user ever
files and routinely ends up in public GitHub issues. Tailscale's
`BUG-<sha256-derived pubID>-<ts>-<64 random bits>` has a stable half that is a
pseudonym already known only to the vendor, plus fresh randomness per report.

For tbx: **no stable host-derived component at all**. A workshop attendee has no
account and no support relationship, so a stable id buys nothing and links their
reports together forever. Use timestamp + randomness only, e.g.
`tbx-20260903T124500Z-9f3a1c72`, and put it in `manifest.json`, the filename, and
the `X-Tbx-Report-Id` header so all three agree.

### Scrubber

Two layers, per sos, and in this order — which is exactly the "known values first,
generic patterns second" already decided in #567, with the survey's evidence behind it:

**Layer 1, known values.** Enumerate from state and the OS, substitute by exact
token, full-word only. sos's username parser is the argument for this ordering: it
deliberately does *not* regex free text, it harvests an authoritative list from
`lastlog` first. tbx's enumerable set is richer than sos's — cluster names, node
names, cluster domains, the host username, `$HOME`, the host hostname, WSL identity,
host interface names, and host LAN IPs all come from state or a syscall.

Borrow four mechanics wholesale:

- **Word-boundary wrapping**, so `demo` does not eat `demo-cp-1` or `democracy`:
  Go has no lookbehind, so use `(^|[^a-zA-Z0-9])(demo)([^a-zA-Z0-9]|$)` with a
  `$1<pseudonym>$3` replacement, or hand-scan with `strings.Index` plus a boundary
  test (cheaper, and avoids the overlapping-match trap where two adjacent tokens
  share a boundary character).
- **Separator equivalence**, sos's `(?:\.|_)` for dots: a cluster domain
  `lab.internal` appears in filenames and log lines as `lab_internal` and
  `lab-internal`. Match all three, emit one pseudonym.
- **Extension stripping**, sos's `strip_exts`: strip a known suffix, substitute the
  stem, reattach, so `demo-cp-1.log` → `cluster0-cp-1.log` rather than being missed.
- **Longest-first ordering**: substitute `demo-worker-1` before `demo`, or the
  shorter token shreds the longer one. sos gets this for free from set-based token
  matching; a Go implementation must sort the known-value table by descending length.

Pseudonyms should be sos-style stable placeholders that survive as topology:
`cluster0`, `cluster0-cp-1`, `cluster0-worker-1`, `cluster0.invalid` for the domain,
`user0`, `host0`. Concretely, on the real daemon log lines this machine produces:

```
2026/08/31 19:35:33 kubernetes client logs go to /Users/oyr/.talosbox/tbxd.k8s.log
2026/08/31 20:03:43 balloon cloudbox/cloudbox-cp-1: target=4096MiB (configured=4096 hostFree=21898 ...)
```

becomes

```
2026/08/31 19:35:33 kubernetes client logs go to /Users/user0/.talosbox/tbxd.k8s.log
2026/08/31 20:03:43 balloon cluster0/cluster0-cp-1: target=4096MiB (configured=4096 hostFree=21898 ...)
```

— the balloon arithmetic, which is the whole diagnostic value of that line, is
untouched.

**Layer 2, generic patterns.** Go-flavoured, adapted from sos's parsers. Note Go's
RE2 has no lookbehind or lookahead, so sos's negative-lookaround guards must become
capture-group boundaries or a post-match check.

```go
// MAC. Boundary chars captured and re-emitted; sentinel OUI so we never
// double-scrub our own output (sos's obfuscated_patterns).
macQuick = regexp.MustCompile(`[0-9a-fA-F]{2}[:\-_][0-9a-fA-F]{2}`)
macRE    = regexp.MustCompile(`(^|[^0-9a-fA-F:.\-])((?:[0-9a-fA-F]{2}[:\-]){5}[0-9a-fA-F]{2})([^0-9a-fA-F:.\-]|$)`)
// -> $1 + "53:4f:53:xx:xx:xx" + $3, skipping ff:ff:ff:ff:ff:ff and 00:00:00:00:00:00

// IPv4, excluding tbx's own 172.30.0.0/16 guest space, which #567 keeps.
ipv4RE = regexp.MustCompile(`(^|[^0-9.\-])((?:[0-9]{1,3}\.){3}[0-9]{1,3})(/[0-9]{1,2})?([^0-9.]|$)`)
// keep: 127.*, 0.*, 169.254.*, 255.*, 224.*-239.*, 172.30.*, 8.8.8.8, 8.8.4.4
// replace others from a counter starting at 100.0.0.0, preserving /prefix

// IPv6. Cheap pre-filter first (sos's _quick_check), then the expensive pattern.
ipv6Quick = regexp.MustCompile(`(?i)::|[0-9a-f]{1,4}:[0-9a-f]{1,4}:`)
// keep ::1, ::/0, fe80::/10 (link-local is diagnostic, not identifying)
// emit under 534f: (global) / fd53: (ULA) so output can never collide with input

// Email, for the optional --message and anything a log line quotes.
emailRE = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}`)
```

**The skip list matters as much as the regexes.** sos's `parser_skip_files` exists
because version strings look exactly like IPv4 addresses, and that lesson was
learned the hard way. tbx's equivalents are unavoidable and everywhere: `v1.13.9`,
`Talos v1.14.0`, `0.2.0`, image digests, `172.30.4.1` (which must survive), and
`protocol=7`. Two defences, both cheap:

- Never IPv4-substitute a match preceded by `v`, `V`, `=` or a digit-and-dot run
  longer than four octets — the leading `[^0-9.\-]` boundary group above handles
  most of it; add an explicit version-token guard for `v?\d+\.\d+\.\d+`.
- Skip the generic pass entirely on files that are known to be version inventories
  (`version.txt`, the `manifest.json` tbx itself wrote).

**Secret material.** Follow Talos's construction argument, not a regex: the builder
never opens `secrets.yaml`, `talosconfig`, `kubeconfig` or the ingress PKI, so
there is nothing to scrub. Then add sos's belt-and-braces for anything that leaks
into a *log line* — a PEM block and a bearer token can be printed by a component
tbx does not control:

```go
pemRE = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]+-----.*?-----END [A-Z ]+-----`)
// -> "-----SCRUBBED PRIVATE KEY-----" : sos keeps the *type* so the reader knows
//    what was removed, which is strictly more useful than a bare placeholder.

// Key/value secrets in yaml or ini form, sos's protect_keys idiom, keys preserved:
secretKV = regexp.MustCompile(`(?im)^(\s*(?:password|token|secret|api[_-]?key|bearer|authorization)\s*[:=]\s*)(.+)$`)
// -> "$1*********"

// URL credentials, keeping the scheme and user:
urlCred = regexp.MustCompile(`([a-z][a-z0-9+.\-]*://)([^:@/\s]+):([^@/\s]+)@`)
// -> "$1$2:******@"
```

Take gh's fixed-width lesson for anything that is genuinely a secret value:
**never length-preserving** — `*********` at a constant width, not
`strings.Repeat("*", len(v))`. Preserve the *type* (`-----SCRUBBED RSA PRIVATE KEY`,
`Bearer ****`) when the type is diagnostic, never the length.

Also take gh's ANSI defence: the bundle concatenates console ring buffers a human
will `cat`, and Talos already reaches for `safeout.Cell` on node-supplied strings
for the same reason. Strip escape sequences from console and log text on the way in.

**Verification pass, fail closed** — already decided in #567, and the survey
supports it as the differentiator. sos explicitly does *not* do this; its disclaimer
admits best-effort coverage instead. tbx can do better precisely because its known-value
set is small and enumerable: after scrubbing, re-scan the assembled bundle for every
known value, and refuse the report naming the file if any survives. That is a claim
no tool in this survey can make, and it is only affordable because the enumerable set
is a dozen strings rather than a whole OS.

### Caps

Talos and kind both have literally none, and it shows: unbounded `journalctl`,
`tailLines = -1`, whole `/var/log` trees. Cap per file and say so. sos's `--log-size`
default of 25 MiB is a reasonable order of magnitude for a whole-OS bundle but far
too generous for tbx — the daemon log on this machine is 464 KB after two weeks.
Suggest 1 MiB per log tail and 256 KiB per console buffer, tail-biased (the end of a
log is where the failure is), with the manifest recording `truncated: true` and the
byte count dropped. Follow Tailscale's truncation marker convention: annotate the
elision inline (`…+<N> bytes dropped`) rather than silently cutting.

### Preview wording

sos's two-part split is the right model: a *pre-run* banner saying what will happen,
and a *post-run* summary saying what to do with the result. Crib its honesty —
"best-effort", "review before sending" — but tbx can make a stronger claim than sos
because of the fail-closed verification pass, so the wording should promise the
enumerable part and disclaim the rest:

```
tbx report

Built a scrubbed diagnostic bundle for this machine:

  /Users/you/.talosbox/reports/tbx-20260903T124500Z-9f3a1c72.tar.gz   412 KiB

  manifest.json           run metadata, collector status, scrubber counts
  doctor.txt              full tbx doctor output
  config/talosbox.yaml    the config in use
  daemon-logs/            tbxd.log, tbxd.k8s.log (last 1 MiB each, truncated)
  clusters/cluster0/      cluster state and console buffers (256 KiB per node)

Removed: your username and home directory, this host's name, its LAN
addresses, interface names and MAC addresses. Cluster, node and domain
names are replaced with stable placeholders (cluster0, cluster0-cp-1) so
the topology still reads. Guest addresses under 172.30.0.0/16 are kept.
The scrubber replaced 47 values across 6 files.

Never collected: secrets.yaml, talosconfig, kubeconfig and ingress keys are
not opened by this command, and your webhook token is not in the bundle.

This is a best-effort scrub of log text tbx does not control. Read the file
before sending it if you are unsure.

Upload to https://reports.example.org/tbx ? [y/N]
```

Four things that wording does deliberately, each earned from the survey:

- **Names the file before asking**, so the answer to "what am I sending" is a path,
  not a promise. Docker only offers this *after* upload; Talos offers it not at all.
- **States what was removed and what was deliberately kept**, with the reason.
  Keeping `172.30.x.x` is a decision that needs saying out loud, exactly as sos says
  it keeps loopback and link-local.
- **Reports the scrubber's own count**, sos's `parsers.<parser>.entries` — evidence
  the pass ran, in a form that leaks nothing.
- **Admits best-effort on the generic layer**, sos's disclaimer, while not
  undercutting the fail-closed guarantee on the enumerable layer.

`--yes` is the `--batch` equivalent: print the banner, skip the prompt. With no
webhook configured, the same preview prints and the last line becomes the path
rather than a question — the local artifact is the deliverable.

### Two smaller things worth copying

- **Record collector failures inside the archive, not just on stderr.** Talos prints
  a failure table and then ships an archive that never mentions it; kind accidentally
  does better by binding stderr into each destination file. A `collectors[]` array in
  `manifest.json` with `status` and `reason` gets it right on purpose.
- **minikube's `--problems`**: filter the collected logs to known-issue lines, capped
  at ten. Independent of bundling, and a good fit for the failure hint that #567
  already plans to print.
