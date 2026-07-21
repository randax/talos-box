# Raw-to-VHDX image pipeline and CoW cloning for a Windows/Hyper-V backend

**Audience:** engineers evaluating a Windows/Hyper-V backend for `talosbox`
(parent: [#48](https://github.com/randax/talos-box/issues/48)). Ticket:
[#53](https://github.com/randax/talos-box/issues/53). Companion to
`docs/SPEC.md` §4, which documents the current macOS pipeline: talosbox's own
Image Factory schematic (`extraKernelArgs: ["console=tty0", "console=hvc0"]`,
mandatory and ordered because Factory's extraKernelArgs *replace* the image's
default console args) plus per-node **APFS `clonefile`** CoW cloning of the
downloaded `metal-arm64.raw.xz`.

## TL;DR recommendation

**Image Factory has no Hyper-V or VHDX output at all** — talosbox would have
to do the raw→VHD(X) step itself, exactly as it already owns the
raw→APFS-clone step on macOS. Keep downloading/decompressing Image Factory's
`metal-amd64.raw.xz` unchanged (the schematic and `extraKernelArgs` bake into
the partitioned raw disk *before* any container format is applied, so this
part of the macOS pipeline design carries over as-is). For the container
format, don't bundle `qemu-img` (GPLv2, no first-party Windows build) and
don't look for a pure-Go VHDX writer (none exists and maintained). Instead,
write a trivial **fixed-format VHD** directly in Go — the format is a raw
sector dump plus a 512-byte footer, small enough to implement natively
(`rubiojr/go-vhd` already demonstrates the "raw2fixed" trick, though it's
archived) — and use that for Generation 1 (BIOS) VMs. If Generation 2
(UEFI/Secure Boot) is required, VHDX is mandatory for the boot disk; promote
the VHD to VHDX with the native `Convert-VHD` cmdlet rather than writing VHDX
directly. For **per-node instant CoW cloning**, skip VHDX differencing disks
— they have real parent-identity fragility (a modified/out-of-band-edited
parent fails an identity check rather than composing safely) — and instead
use **ReFS block cloning** (`FSCTL_DUPLICATE_EXTENTS_TO_FILE`) directly on
the parent VHD/VHDX file, provided it sits on a ReFS volume (a Dev Drive,
ReFS since Windows 11, works). Microsoft explicitly documents block cloning
as dramatically accelerating `.vhdx` operations, and it produces an
independent, fully-writable copy — the closest Windows analogue to APFS
`clonefile`, with none of the differencing-disk parent-chain risk.
Confidence: **high** on the "Image Factory has no VHDX output" and "ReFS
block cloning is the right CoW primitive" claims (both directly verified
against source/docs); **medium** on the Gen2 console-arg analogue to
`console=hvc0` (unverified — see Open Questions).

## Facts

### 1. Does Image Factory offer Hyper-V-ready artifacts, and does `extraKernelArgs` apply to them?

- Image Factory's imager defines exactly four `DiskFormat` values: `raw`,
  `qcow2`, `vhd` (constant `DiskFormatVPC`), and `ova` — **there is no VHDX
  format at all**. ([pkg/imager/profile/output.go](https://github.com/siderolabs/talos/blob/main/pkg/imager/profile/output.go))
- The only built-in profile using `vhd` is `azure`/`secureboot-azure`,
  configured as `DiskFormat: DiskFormatVPC, DiskFormatOptions:
  "subformat=fixed,force_size"` — a **fixed legacy VHD**, not VHDX. There is
  no `hyperv` profile in the default catalog. A source search of both
  `siderolabs/talos` and `siderolabs/image-factory` for `vhdx`/`hyper-v`
  returns no relevant hits. **Verified directly against source: Image
  Factory does not offer, and never has offered, a Hyper-V-ready VHDX
  output.** ([pkg/imager/profile/default.go](https://github.com/siderolabs/talos/blob/main/pkg/imager/profile/default.go),
  [code search](https://github.com/search?q=repo%3Asiderolabs%2Ftalos+vhdx&type=code))
- The VHD conversion is a post-processing shellout to the external
  `qemu-img` binary (`qemu-img convert -f <in> -O <out> -o <options> src
  dest`) applied to the already-built raw partitioned disk image.
  ([pkg/imager/qemuimg/qemuimg.go](https://github.com/siderolabs/talos/blob/main/pkg/imager/qemuimg/qemuimg.go))
  **Inference** (the pipeline shape necessarily implies this, though it's not
  stated explicitly): the container format is a final wrapping step over the
  same partitioned raw disk, so **the schematic/`extraKernelArgs` mechanism
  is format-agnostic** — it bakes into the raw disk before any VHD/VHDX
  wrapping, the same way it already does before the APFS clone step.
- Hyper-V **Generation 2** VMs only support VHDX for the boot disk;
  Generation 1 VMs use VHD. ("Generation 2 virtual machines only support
  VHDX format virtual hard drives... Use generation 1 for VHD and generation
  2 for VHDX.") ([Should I create a generation 1 or 2 VM in Hyper-V? — Microsoft Learn](https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v/plan/should-i-create-a-generation-1-or-2-virtual-machine-in-hyper-v))
- Generation 2 removes legacy emulated devices, including legacy COM ports,
  in favor of synthetic/VMBus devices — analogous to the vz/hvc0 situation,
  Gen2 needs its own console mechanism, and the Gen1 `ttyS0`-style legacy
  serial path doesn't apply. **Not verified against a primary source**:
  whether Linux's `hvc0` driver (as used for vz) is also the correct
  boot-console path under Hyper-V Gen2, or whether Hyper-V needs a different
  arg (e.g. a VMBus-backed serial console) — see Open Questions.

### 2. raw→VHD/VHDX conversion tooling usable from a Go CLI

- `Convert-VHD` **only converts between `.vhd` and `.vhdx`** ("The format is
  determined by the file name extension of the specified files, either
  .vhdx or .vhd") — it **cannot ingest an arbitrary raw disk image**; the
  input must already be a VHD or VHDX file, and conversion is an offline
  operation (source must not be attached).
  ([Convert-VHD — Microsoft Learn](https://learn.microsoft.com/en-us/powershell/module/hyper-v/convert-vhd?view=windowsserver2025-ps))
- The Hyper-V role **cannot be installed on Windows 10/11 Home** — it
  requires Pro or Enterprise, a SLAT-capable 64-bit CPU, VM Monitor Mode
  Extensions, and 4 GB+ RAM.
  ([Install Hyper-V — Microsoft Learn](https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v/get-started/install-hyper-v))
- On Windows **Server**, `-IncludeManagementTools` on Server Core installs
  only the Hyper-V PowerShell module (i.e. `Convert-VHD`/`New-VHD`) without
  the full hypervisor role, confirming the cmdlets and the hypervisor
  platform are separable components — at least on Server. **Open
  question**: no primary-source confirmation found of an equivalent
  management-tools-only path on Windows 11 client (vs. the combined
  "Hyper-V" optional feature) — see Open Questions.
- QEMU overall is **GPLv2** ("QEMU is released under the GNU General Public
  License, version 2"); the LICENSE file lists a few GPLv2-only
  subdirectories and notes TCG is mostly BSD/MIT, but does **not** carve out
  `qemu-img` or the block layer as separately/permissively licensed — so
  `qemu-img` is governed by the project-wide GPLv2.
  ([QEMU license page](https://www.qemu.org/docs/master/about/license.html),
  [QEMU LICENSE file](https://github.com/qemu/qemu/blob/master/LICENSE))
- The **official qemu.org download page does not distribute a first-party
  Windows build**; it hands off to a third party ("Stefan Weil provides
  binaries and installers for both 32-bit and 64-bit Windows", hosted at
  `qemu.weilnetz.de`) plus an MSYS2-based alternative.
  ([Download QEMU](https://www.qemu.org/download/))
- **Inference** (not a primary-source legal statement): shelling out to a
  separately-distributed `qemu-img.exe` subprocess, rather than
  statically/dynamically linking it, is generally treated as "mere
  aggregation" under GPLv2 and would not itself force the calling Go CLI to
  be GPL-licensed — but *redistributing* the `qemu-img.exe` binary (bundling
  it in an installer) carries GPLv2 redistribution obligations regardless of
  the caller's own license.
- Pure-Go VHD/VHDX library survey:
  - `Velocidex/go-vhdx` — **read-only parser only**, no write/create
    support, but pure Go and based on the published VHDX spec.
    ([Velocidex/go-vhdx](https://github.com/Velocidex/go-vhdx))
  - `microsoft/go-winio`'s `vhd` package — not a format implementation; a
    `//go:build windows`-only wrapper around `virtdisk.dll` syscalls
    (`CreateVirtualDisk`, `OpenVirtualDisk`, `AttachVirtualDisk`). Only
    compiles/runs on Windows and delegates the actual construction to the OS.
    ([go-winio vhd.go](https://github.com/microsoft/go-winio/blob/main/vhd/vhd.go))
  - `rubiojr/go-vhd` — pure Go, **does write fixed-format VHD** directly
    (`govhd create foo.vhd 80GiB`, `go-vhd raw2fixed img.raw`); its tooling
    dumps a real VHD footer (`Cookie: 0x636f6e6563746978` = `"conectix"`,
    plus geometry/disk-type/checksum fields), corroborating the "VHD footer
    is simple" claim. **VHD only, no VHDX; archived (read-only) since
    November 2021.** ([rubiojr/go-vhd](https://github.com/rubiojr/go-vhd))
  - `volfasa/vhdx-cli` — README claims a Go CLI to create/mount/expand VHDX,
    but the repository tree contains **no Go source files at all**. Treat
    as vaporware, not a real option.
    ([volfasa/vhdx-cli](https://github.com/volfasa/vhdx-cli))
  - **No working, maintained pure-Go VHDX writer exists.**
- Could not directly open the primary **[MS-VHD]** "Virtual Hard Disk Image
  Format Specification" — it's distributed only as a legacy `.doc` file from
  the Microsoft Download Center, which does not render as fetchable text.
  ([VHD Specifications download page](https://www.microsoft.com/en-us/download/details.aspx?id=23850))
  The "trivial 512-byte fixed footer, `conectix` cookie" claim is
  corroborated only indirectly (via `rubiojr/go-vhd`'s runtime footer dump
  above), not by reading the spec text itself — see Open Questions.
- The **MS-VHDX** spec (readable as an OpenSpecs page) confirms VHDX
  supports three disk types — Fixed, Dynamic, and Differencing.
  ([MS-VHDX Overview](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-vhdx/83f6b700-6216-40f0-aa99-9fcb421206e2))

### 3. VHDX differencing disks vs. ReFS block cloning vs. Dev Drive vs. NTFS sparse files

**Differencing disk semantics.** MS-VHDX defines a differencing VHDX as
representing "the current state of the virtual hard disk as a set of
modified blocks in comparison to a parent virtual hard disk file. Any new
write to the virtual disk is captured in the latest child virtual hard disk.
A read... is satisfied by looking for that virtual offset on the latest
child... and traversing all the way to the parent if needed." Standard
block-level CoW with parent read-passthrough, confirmed directly from the
spec.
([MS-VHDX Overview](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-vhdx/83f6b700-6216-40f0-aa99-9fcb421206e2))

**Parent identity / invalidation.** The VHDX Parent Locator stores a
`DataWriteGuid` entry: "When a differencing VHDX file is created, the
implementation MUST populate the parent's DataWriteGuid field in this field.
When opening the parent VHDX file of a differencing VHDX, the implementation
MUST verify that the DataWriteGuid field of the parent's header matches" the
stored value. The format itself mandates an identity check on open — a
parent modified out-of-band (which changes its own DataWriteGuid) fails this
check rather than silently composing/corrupting. Parent *location* is
resolved via `relative_path`/`volume_path`/`absolute_win32_path` entries in a
defined fallback order, and a successful open updates stale path entries —
so **moving** the parent is tolerated, but altering its **contents/identity**
is not.
([MS-VHDX Parent Locator](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-vhdx/b6332a98-624d-46b8-bd0e-b77b573662f9))
Microsoft's Hyper-V troubleshooting guide confirms this in practice: broken
parent/child relationships surface as "identifier mismatch"/"broken chain"
errors requiring manual repair (`Set-VHD -ParentPath`, or merging the
chain) — Hyper-V detects the mismatch rather than silently corrupting data.
([Troubleshooting Hyper-V snapshots, checkpoints, and differencing disks](https://learn.microsoft.com/en-us/troubleshoot/windows-server/virtualization/hyper-v-snapshots-checkpoints-differencing-disks))

**Performance/reliability guidance.** Microsoft's Best Practices Analyzer
warns specifically against **VHD-format** (not VHDX) differencing disks in
production: "VHD-format differencing virtual hard disks could experience
consistency issues if a power failure occurs... convert the chain... to the
VHDX format... (The VHDX format has reliability mechanisms that help protect
the disk from corruptions due to power failures.)" — a first-party statement
that VHDX is more crash-resilient than legacy VHD as a differencing format,
though it gives no quantified performance-overhead numbers for VHDX
differencing specifically.
([Avoid VHD-format differencing disks in production](https://learn.microsoft.com/en-us/previous-versions/windows-server/it-pro/windows-server-2016/virtualization/hyper-v/best-practices-analyzer/avoid-using-vhd-format-differencing-virtual-hard-disks-on-virtual-machines))

**ReFS block cloning.** `FSCTL_DUPLICATE_EXTENTS_TO_FILE` on ReFS performs
cloning as a pure metadata operation (remapping VCN→LCN with reference
counts) — "no physical data is read, nor is the physical data in the
destination file overwritten," with allocate-on-write duplicating only
modified clusters on subsequent writes. Microsoft explicitly calls out VM
disk use: **"This improvement also benefits virtualization workloads, as
`.vhdx` checkpoint merge operations are dramatically accelerated when using
block clone operations."** This directly confirms block cloning works on
VHDX files on a ReFS volume, yielding a real independent copy without
differencing-disk parent-chain fragility. Restrictions: source/destination
must be on the **same ReFS volume**, cluster-aligned regions <4 GB each, max
8175 files referencing the same physical region, matching Integrity Streams
settings, and the volume must be formatted on Windows Server 2016+. Native
transparent block-cloning in ordinary copy operations arrived with
**Windows 11 24H2 / Windows Server 2025**.
([Block cloning on ReFS — Microsoft Learn](https://learn.microsoft.com/en-us/windows-server/storage/refs/block-cloning))

**Dev Drive.** Dev Drive is ReFS-backed, and "Beginning in Windows 11 24H2 &
Windows Server 2025, Block cloning is now supported on Dev Drive... free
performance benefits whenever you copy a file using Dev Drive." Dev Drive is
scoped by Microsoft to source repos, package caches, and build
output/intermediates ("do not recommend installing applications on a Dev
Drive"). The doc does not explicitly bless or warn against hosting *other
VMs'* VHD/VHDX files there (as opposed to using VHD/VHDX as the Dev Drive's
own backing container, which is a separate, orthogonal use) — inferred to
be fine since it's still ReFS underneath, but not a first-party-stated
scenario; see Open Questions.
([Set up a Dev Drive — Microsoft Learn](https://learn.microsoft.com/en-us/windows/dev-drive/))

**NTFS sparse files.** `FSCTL_SET_SPARSE` marks a file sparse so "large
ranges of zeros may not require disk allocation," with space allocated only
as nonzero data is written. This gives **space savings for zero-filled
regions only** — nothing in Microsoft's sparse-file documentation describes
reference-counted, cross-file block sharing the way ReFS block cloning does.
**Inference** (by comparison with the ReFS block-cloning doc, which frames
the feature as ReFS-exclusive): NTFS does not provide APFS-clonefile-style
instant CoW cloning between independent files — a plain copy on NTFS, sparse
or not, is a real, non-shared duplicate.
([Sparse File Operations — Microsoft Learn](https://learn.microsoft.com/en-us/windows/win32/fileio/sparse-file-operations))

## Open questions

1. **Console mechanism for Hyper-V Gen2 Linux guests.** No primary Microsoft
   or kernel source confirms whether `console=hvc0` (or some other specific
   arg) is the correct boot-console path for Hyper-V Generation 2 VMs, the
   way it's confirmed for vz. Gen2 removing legacy COM ports is confirmed;
   the exact replacement console driver/arg for Talos's kernel cmdline is
   not.
2. **Windows 11 client management-tools-only Hyper-V install.** Confirmed
   for Windows Server (`-IncludeManagementTools` on Server Core); no
   primary-source confirmation of an equivalent path on Windows 11 client to
   get `Convert-VHD`/`New-VHD` without enabling the full Hyper-V hypervisor
   feature.
3. **[MS-VHD] fixed-VHD footer spec.** Could not open the actual [MS-VHD]
   spec document (legacy `.doc` file, not fetchable as text). The
   "trivial 512-byte fixed footer" claim rests on `rubiojr/go-vhd`'s runtime
   footer dump, not on reading the primary spec text.
4. **Dev Drive as a host for other VMs' VHD/VHDX files.** Not explicitly
   addressed by Microsoft's Dev Drive docs, which scope Dev Drive to
   source/build/package-cache workloads.
5. **No working pure-Go VHDX writer.** Confirmed absence, but this could
   change; worth re-checking before committing to the "write fixed VHD,
   promote via `Convert-VHD`" plan long-term.
6. **Quantified differencing-disk performance overhead.** Microsoft
   documents VHD-format differencing-disk reliability risk and a
   VHDX-over-VHD preference, but no first-party IOPS/latency numbers
   comparing VHDX differencing disks to plain fixed/dynamic VHDX are
   available.
