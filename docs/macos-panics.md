# macOS kernel panics: `Kernel tag check fault` (#513)

Some hosts kernel-panic — the whole Mac reboots — around talos-box cluster
lifecycle transitions, with a panic report naming `tbxd` and the signature
`Kernel tag check fault` (`esr 0x96000011`). This page records the confirmed
diagnosis, who is affected, and what to do about it.

**The bug is in macOS, not in talos-box.** No tbx setting fixes it; two
workarounds remove the exposure entirely.

## Diagnosis

Symbolizing the panic backtraces against the matching kernel image
(xnu-12377.161.14~5, T6050) places every fault at the same instruction:
`cfil_acquire_sockbuf` reading `cfil_info->cfi_flags` — XNU's **socket
content-filter** state — through a pointer whose allocation was already freed.
The recovered call chain is:

```
close() syscall (tbxd) → soclose → soclose_locked
  → cfil_sock_close_wait → cfil_sock_is_closed
  → cfil_acquire_sockbuf → freed cfil_info
```

In plain terms: when a **content-filter Network Extension** (GlobalProtect,
Zscaler, Netskope, Cisco Secure Client, …) is active, XNU attaches filter
state to TCP sockets. A kernel bug can leave that state dangling, and the
next process to close such a socket dereferences it. `tbxd` opens and closes
many TCP sockets around cluster create/destroy — Talos API clients, registry
mirror transports, DNS — which is why the panics cluster at lifecycle
transitions and why `tbxd` is the named task. Any sufficiently socket-busy
process could trigger the same fault.

Three ingredients are all required:

1. **A content-filter system extension is active.** No filter, no `cfil`
   state, no bug. `tbx doctor`'s `security-inventory` lines list activated
   extensions.
2. **The Mac's silicon enforces kernel memory tagging (MTE).** Only
   M5-generation Macs do; that is what turns the stale read into a panic.
   Earlier Apple Silicon (M1–M4) executes the same use-after-free
   *silently* — "it works on my M1" means undetected, not absent.
3. **TCP sockets closing** — normal operation for tbxd and much else.

`tbx doctor` prints a `WARN security-inventory` line when ingredients 1 and 2
are both present.

### What it is not

The #513 investigation initially suspected the virtio memory balloon and VZ
teardown ordering, and shipped `TBX_DISABLE_BALLOON` (v0.1.5) plus teardown
serialization as candidate mitigations. A reporter reproduced the identical
panic with the balloon fully disabled, and the symbolized backtraces ruled
out the balloon, Virtualization.framework teardown, and the vmnet
file-descriptor path (an AF_UNIX socket, which the content filter never
touches). `TBX_DISABLE_BALLOON` remains a legitimate memory-management choice
(see [guest memory](guest-memory.md)); it does not prevent these panics.

## What to do

In order of preference:

1. **Exempt tbx traffic from the filter.** If your filter product supports
   per-process or per-subnet exclusions (fleet policy), excluding tbxd's
   traffic removes the `cfil` state from its sockets.
2. **Deactivate the content-filter extension** while running talos-box
   workloads. Disconnecting the VPN is usually **not** enough — the filter
   extension can stay loaded and enforcing. Verify with
   `systemextensionsctl list`: the filter must not be `[activated enabled]`.
3. **Update macOS** once Apple ships a fix, and keep the filter vendor's
   client current — vendor updates can change the kernel-side lifecycle
   enough to dodge the race.
4. Failing all of the above, run on pre-M5 hardware. The underlying
   use-after-free still occurs; the hardware simply cannot trap it.

## Reporting

If you hit this panic, the useful artifacts are the `.panic` file from
`/Library/Logs/DiagnosticReports/`, the output of `systemextensionsctl list`
and `profiles show -type configuration`, and your macOS build. File with
Apple (Feedback Assistant) referencing the fault site above, and with your
filter vendor. The talos-box side is tracked in
[#513](https://github.com/randax/talos-box/issues/513).
