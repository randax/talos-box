# Windows hypervisor backends drivable from Go

Research for wayfinder ticket [#50](https://github.com/randax/talos-box/issues/50) (part of #48). Scope: what can a Go
daemon use to programmatically create/manage VMs on Windows 11 Pro/Enterprise x64, and what does each option cost in
API maturity, privilege, corporate-fleet availability, and licensing.

Status of claims: sentences with a bracketed source are verified against the cited primary/near-primary document.
Sentences marked **[inference]** are this author's synthesis/judgment, not a directly-cited fact — treat them as
starting hypotheses to validate against a real corporate Windows 11 image before committing to a design.

## TL;DR — leading candidates

1. **Hyper-V via PowerShell exec (shelling out to the `Hyper-V` PowerShell module)** is the most proven path: every
   surveyed precedent project (minikube, multipass) that talks to Hyper-V does so this way, not through raw WMI
   ([minikube hyperv driver docs](https://minikube.sigs.k8s.io/docs/drivers/hyperv/)). It trades API elegance for
   using exactly the surface Microsoft documents and supports, and for reusing the error-handling folklore already
   written by those projects.
2. **QEMU + WHPX**, driven the same way (shell out to `qemu-system-x86_64.exe -accel whpx`), is the strongest
   *cross-platform-code-reuse* candidate if talos-box already drives QEMU on Linux/macOS — it's the same accelerator
   model as Apple's HVF or Linux KVM from QEMU's point of view, and it's a built-in, license-free Windows optional
   feature ([QEMU WHPX docs](https://www.qemu.org/docs/master/system/whpx.html)).
3. **Native WMI/`Msvm_*` from Go** is technically possible but not recommended as a first choice today: the
   Microsoft-maintained `microsoft/wmi` Go module is real and active, but no precedent project actually drives Hyper-V
   VM lifecycle through it, and the one Go library purpose-built for it (`gabriel-samfira/go-wmi`) ships with a README
   that says "Not ready for usage. May not work at all." ([go-wmi repo](https://github.com/gabriel-samfira/go-wmi)).

VirtualBox and a raw HCS/WSL2-utility-VM approach are both viable but come with sharper corporate-fleet caveats
(licensing and unstable/internal APIs respectively) — see their sections below.

## Comparison table

| Candidate | Go API surface | Privilege to enable | Privilege at runtime | Corporate Windows 11 Pro/Enterprise availability | Licensing/redistribution | Precedent pain |
|---|---|---|---|---|---|---|
| Hyper-V native WMI (`Msvm_*`, root\virtualization\v2) | `microsoft/wmi` (Microsoft, MIT, active, generic WMI codegen — not Hyper-V-lifecycle-specific docs); `gabriel-samfira/go-wmi` (explicitly not production-ready); generic WQL libs (`StackExchange/wmi`/forks) don't cover Msvm methods | Admin, one-time, to enable the `Microsoft-Hyper-V` Windows feature + reboot [minikube docs](https://minikube.sigs.k8s.io/docs/drivers/hyperv/) | Membership in local **Hyper-V Administrators** group (plus `root\interop` WMI permissions + `WinRMRemoteWMIUsers__` for remote/non-admin scenarios); some WMI-driven create operations still error under non-admin ([Microsoft Q&A / IBM docs on non-admin Hyper-V access](https://www.ibm.com/docs/en/capm?topic=cmhvm-adding-non-administrator-user-in-hyper-v-administrator-users-group)) | Requires Pro/Enterprise/Education edition [minikube docs](https://minikube.sigs.k8s.io/docs/drivers/hyperv/); often disabled by corporate image/GPO for compatibility with other type-2 hypervisors or MDM-locked Windows Features **[inference from GPO/service-disable guidance]** ([Microsoft: block virtualization features on specific computers](https://learn.microsoft.com/en-us/troubleshoot/windows-server/virtualization/block-users-from-running-virtualization-features-on-specific-computers)) | Hyper-V itself is a free, in-box OS feature — no separate license | No precedent project drives Hyper-V lifecycle through raw WMI in production; the closest is Cloudbase/Gabriel Samfira's experimental `go-wmi`, self-described as not ready |
| Hyper-V via PowerShell exec (`New-VM`, `Set-VM*`, `Start-VM`, …) | None needed beyond `os/exec` + stdout/stderr/JSON parsing; no typed Go bindings, just shelling to `powershell.exe` | Same as above (enable feature) | Effectively admin/Hyper-V-Administrators in practice — minikube's docs and issue tracker show creation failing under insufficient privilege | Same edition/GPO constraints as above | Free | **Documented, real pain**: "Hyper-V PowerShell Module is not available" errors ([minikube #2634](https://github.com/kubernetes/minikube/issues/2634)), `New-VM` `VirtualizationException` failures ([minikube #6104](https://github.com/kubernetes/minikube/issues/6104)), stuck `.vmcx` files blocking restart requiring `minikube delete --all --purge` ([minikube hyperv driver docs](https://minikube.sigs.k8s.io/docs/drivers/hyperv/)) |
| QEMU + WHPX | None Hyper-V-specific; talos-box would drive `qemu-system-x86_64.exe` the same way it presumably already drives QEMU elsewhere (CLI/QMP), with `-accel whpx` | Admin, one-time, to enable the **Windows Hypervisor Platform** optional feature (`optionalfeatures.exe`, Server Manager, or `DISM /Online /Enable-Feature /FeatureName:HypervisorPlatform /All`) ([QEMU WHPX docs](https://www.qemu.org/docs/master/system/whpx.html)) | Not documented as needing ongoing admin for a running QEMU process once the feature is enabled — **[inference, unverified against a real locked-down fleet]** | Requires Windows 10 2004+ / Windows 11 (x86_64); needs Intel VT-x/AMD-V in firmware ([QEMU WHPX docs](https://www.qemu.org/docs/master/system/whpx.html), [Windows Hypervisor Platform overview](https://www.qemu.org/docs/master/system/whpx.html)); WHPX and Hyper-V share the same underlying hypervisor so no VBS/Credential-Guard exclusivity conflict the way VMware/VirtualBox's classic engines have **[inference]** | QEMU is GPLv2, free and redistributable | No major precedent (minikube/kind/podman/multipass) ships a QEMU+WHPX Windows driver today; QEMU's own docs list known limitations (legacy VGA perf, some MMIO instruction gaps on x86_64; SVE/SME gaps on arm64) ([QEMU WHPX docs](https://www.qemu.org/docs/master/system/whpx.html)) |
| WSL2 / HCS (Host Compute Service) via `Microsoft/hcsshim` | `Microsoft/hcsshim` (Microsoft, MIT, pre-1.0 at v0.14.x, 644+ importers); public `hcs`/`hcn`/`computestorage` packages are usable, but the Utility-VM helper code (`internal/uvm`, LCOW boot) lives under Go's `internal/` path and is **not importable outside the hcsshim module** — using it for a general-purpose VM would mean hand-building raw HCS JSON schema documents against an internal/undocumented contract, not calling a public API **[verified: package is under internal/, inference: consequence for third-party use]** | Admin, one-time, to enable "Virtual Machine Platform" optional feature (same family as WSL2's requirement) [WSL architecture docs](https://learn.microsoft.com/en-us/windows/wsl/about) | Unclear/undocumented for non-container HCS use — open question | WSL2/Virtual Machine Platform ships on all Desktop SKUs incl. Home [WSL about docs](https://learn.microsoft.com/en-us/windows/wsl/about); increasingly pre-provisioned in corporate dev images because of Docker Desktop's WSL2 requirement **[inference]** | hcsshim is MIT, free | Used in production by Moby/containerd/BuildKit for Windows+Linux containers, but not by any of the four named precedent projects for general VM lifecycle |
| VirtualBox | Shell out to `VBoxManage`; no Go-native bindings found in this survey | Admin required to install (kernel driver) | Multipass runs its Windows service as SYSTEM, causing instance visibility quirks — viewing VMs via `VBoxManage`/VirtualBox GUI as a normal user requires `PsExec -s` ([Multipass driver docs](https://documentation.ubuntu.com/multipass/latest/how-to-guides/customise-multipass/set-up-the-driver/)) | Available on Home too (multipass recommends VirtualBox specifically for Home edition where Hyper-V is unavailable) ([Multipass driver docs](https://documentation.ubuntu.com/multipass/latest/how-to-guides/customise-multipass/set-up-the-driver/)); often disfavored/blocked on corporate fleets due to conflicts with VBS-backed security tooling and kernel-driver installs requiring admin **[inference]** | Base package GPLv2/free, but **Extension Pack requires a paid Enterprise license for any commercial/business/government use** beyond a 30-day evaluation — the free PUEL license explicitly excludes commercial/organizational use ([VirtualBox Licensing FAQ](https://www.virtualbox.org/wiki/Licensing_FAQ), [VirtualBox PUEL](https://www.virtualbox.org/wiki/VirtualBox_PUEL)). Several Extension-Pack-gated features (RDP, USB 2.0/3.0, PXE boot ROM, disk encryption) may matter to a hypervisor product. | Multipass falls back to VirtualBox on Windows Home; corporate procurement of the Enterprise Extension Pack is a real, non-trivial approval step |
| WSL2 as a "VM" per se (not via HCS/Go) | n/a — no general Go API; would mean shelling to `wsl.exe` | Admin, one-time, to enable the feature | n/a | Broad — ships on all SKUs | Free | kind explicitly documents running *inside* WSL2 rather than driving WSL2 as a VM backend from the host ([kind WSL2 docs](https://kind.sigs.k8s.io/docs/user/using-wsl2/)); this is a fundamentally different usage pattern (dev environment, not "manage a VM for someone else") and is not a real candidate for a Go daemon that needs to own VM lifecycle |

## Per-candidate detail

### 1. Native Hyper-V via WMI (`Msvm_*`, `root\virtualization\v2`)

- The Hyper-V object model is exposed through WMI in the `root\virtualization\v2` namespace, with `Msvm_ComputerSystem`
  representing hosts/VMs and `Msvm_VirtualSystemManagementService` as the entry point for lifecycle operations
  ([Msvm_VirtualSystemManagementService — Microsoft Learn](https://learn.microsoft.com/en-us/windows/win32/hyperv_v2/msvm-virtualsystemmanagementservice)).
- Go options found:
  - **`microsoft/wmi`** — Microsoft-maintained, MIT-licensed, actively released (v0.43.0 as of this research), generic
    Go/WMI/COM bindings auto-generated per Windows/Server version (`server2019`, etc.), including a
    `root/virtualization/v2` package. It is a generic WMI codegen tool, not a Hyper-V-lifecycle-specific SDK with
    example workflows for create/start/stop/snapshot — using it for Hyper-V still means hand-driving the raw
    `Msvm_*` method-call protocol yourself ([microsoft/wmi](https://github.com/microsoft/wmi)).
  - **`gabriel-samfira/go-wmi`** — purpose-built around a `virt/vm` package for Hyper-V, authored by a Cloudbase
    Solutions engineer (Cloudbase does real Windows/Hyper-V infrastructure work), but the repository's own README
    states "Not ready for usage. May not work at all," has no tagged releases, and only 38 commits
    ([go-wmi](https://github.com/gabriel-samfira/go-wmi)).
  - **`StackExchange/wmi`** and forks (`yusufpapurcu/wmi`, `bi-zone/wmi`) are generic WQL query libraries (good for
    reading WMI data), not Hyper-V method-invocation SDKs; upstream StackExchange/wmi is explicitly unmaintained,
    with users pointed to the `yusufpapurcu` fork.
  - **`go-ole`** underlies most of the above — mature, semver'd COM/OLE bindings for Go, but a low-level building
    block, not a Hyper-V API.
- Privilege: enabling Hyper-V itself needs admin + reboot. At runtime, non-admin access is possible in principle via
  the **Hyper-V Administrators** local group plus `root\interop` WMI permissions and `WinRMRemoteWMIUsers__` group
  membership for remote scenarios, but some create-VM operations still fail under non-admin even with those grants
  ([IBM docs on non-admin Hyper-V access](https://www.ibm.com/docs/en/capm?topic=cmhvm-adding-non-administrator-user-in-hyper-v-administrator-users-group)).
- No surveyed precedent (minikube, kind, podman, multipass) drives Hyper-V lifecycle through raw WMI from Go/any
  language in production — they all go through PowerShell.

### 2. Hyper-V via PowerShell exec

- This is what minikube's `hyperv` driver, Docker Machine's Hyper-V driver, and Vagrant's Hyper-V provider all do:
  shell out to `powershell.exe` and invoke the in-box `Hyper-V` module (`New-VM`, `Set-VMProcessor`, `Start-VM`, etc.)
  ([minikube hyperv driver](https://minikube.sigs.k8s.io/docs/drivers/hyperv/)).
- Requirements per minikube's own docs: 64-bit Windows 10/11 Enterprise, Pro, or Education; Hyper-V feature enabled
  via `Enable-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V -All` as Administrator, with a reboot
  ([minikube hyperv driver](https://minikube.sigs.k8s.io/docs/drivers/hyperv/)).
- Documented pain, directly from the minikube issue tracker and docs:
  - "Hyper-V PowerShell Module is not available" when the module isn't present/importable
    ([minikube #2634](https://github.com/kubernetes/minikube/issues/2634)).
  - `New-VM` throwing `VirtualizationException`/`NotSpecified` errors with no actionable message
    ([minikube #6104](https://github.com/kubernetes/minikube/issues/6104)).
  - VM start failures traced to stale `.vmcx` configuration files left locked by a defunct process, requiring
    `minikube delete --all --purge` and manual handle-hunting to clear
    ([minikube hyperv driver docs](https://minikube.sigs.k8s.io/docs/drivers/hyperv/)).
  - Virtual switch management is a persistent source of friction — the driver needs an existing/created
    `New-VMSwitch`, and flag combinations (`--hyperv-virtual-switch`, external switch auto-create) are a common
    misconfiguration point.
- Multipass's default Windows driver is also Hyper-V ([Multipass driver docs](https://documentation.ubuntu.com/multipass/latest/how-to-guides/customise-multipass/set-up-the-driver/)), reinforcing that PowerShell-driven Hyper-V is the field-tested default across precedent projects, not raw WMI.

### 3. QEMU + WHPX

- WHPX (Windows Hypervisor Platform) is QEMU's accelerator for Windows hosts, analogous to KVM on Linux or HVF on
  macOS — it requires the "Windows Hypervisor Platform" optional feature, enableable via `optionalfeatures.exe`,
  Server Manager, or `DISM /Online /Enable-Feature /FeatureName:HypervisorPlatform /All`
  ([QEMU WHPX docs](https://www.qemu.org/docs/master/system/whpx.html)).
- Host OS support: x86_64 guests need Windows 10 version 2004+ (earlier untested); arm64 guests need Windows 11 24H2
  with specific 2025 updates ([QEMU WHPX docs](https://www.qemu.org/docs/master/system/whpx.html)) — Windows 11 Pro/Enterprise x64 as named in the ticket is well within the supported range.
- No Go bindings to the underlying `WinHvPlatform.dll` API were found in this survey; the only concrete artifact is a
  Rust binding (`Zero-Tang/whpx`) — driving WHPX from Go in practice means either (a) shelling out to
  `qemu-system-x86_64.exe -accel whpx ...` (documented, supported path) or (b) hand-writing `syscall`/`golang.org/x/sys/windows`
  bindings directly against `WinHvPlatform.dll`, which is unexplored territory with no existing library.
- Known QEMU/WHPX limitations per the official docs: poor legacy VGA-mode performance (recommend modern graphics
  modes or TCG instead), unsupported MMX/SSE/AVX MMIO-access instruction emulation, and on Windows 10 a PIC-interrupt
  wake bug requiring `-M q35,pic=off` with UEFI ([QEMU WHPX docs](https://www.qemu.org/docs/master/system/whpx.html)).
- Licensing: QEMU is GPLv2, free to redistribute; the WHPX feature itself is a free in-box Windows component.
- No precedent project (minikube/kind/podman/multipass) currently ships a QEMU+WHPX Windows driver in the sources
  reviewed — podman's own provider list does include a `qemu` provider, but Windows defaults to `wsl`/`hyperv`
  ([podman-machine docs](https://docs.podman.io/en/latest/markdown/podman-machine.1.html)) — QEMU is podman's Linux-host default, with WHPX-on-Windows specifically not called out as a shipped, supported provider path in the docs reviewed.

### 4. WSL2 / Host Compute Service (HCS) via `Microsoft/hcsshim`

- WSL2 itself is architecturally "a Linux kernel inside a lightweight utility VM," running on a subset of Hyper-V
  called "Virtual Machine Platform," available on all Desktop SKUs including Home
  ([WSL "About" docs](https://learn.microsoft.com/en-us/windows/wsl/about)).
- `Microsoft/hcsshim` is the Go library that Moby/containerd/BuildKit use to drive the same underlying Host Compute
  Service API that WSL2, Windows containers, and Hyper-V-isolated containers all sit on top of. It is MIT-licensed,
  Microsoft-maintained, and has 644+ known importers, but is still pre-1.0 (v0.14.x) with no stability guarantee
  (`pkg.go.dev/github.com/Microsoft/hcsshim`).
- Important caveat found during this research: the Utility-VM creation helpers (`internal/uvm`, LCOW boot support for
  a general Linux root filesystem/kernel/initrd) live under hcsshim's `internal/` Go package path, which Go's tooling
  refuses to let external modules import. Any use of hcsshim for a *general-purpose* Linux VM (as opposed to a
  container-runtime-scoped compute system) would mean either vendoring hcsshim's internal code (fragile across
  upstream changes) or reverse-engineering the raw HCS JSON schema documents and driving the public `hcs` package
  directly, an unsupported/undocumented path for that use case **[inference from the internal/ package layout]**.
- No named precedent project (minikube/kind/podman/multipass) uses hcsshim directly for general VM lifecycle in the
  sources reviewed; it's exclusively a container-runtime-internals library in the wild.

### 5. VirtualBox

- Multipass documents VirtualBox as its Windows fallback for Home edition (where Hyper-V isn't available), with
  Hyper-V remaining the default/preferred driver on Pro
  ([Multipass driver docs](https://documentation.ubuntu.com/multipass/latest/how-to-guides/customise-multipass/set-up-the-driver/)).
- Multipass's Windows service runs as the SYSTEM account, which means VMs aren't visible in the VirtualBox GUI or via
  `VBoxManage` as a normal user without `PsExec -s` — a concrete, documented ergonomics cost of this backend
  ([Multipass driver docs](https://documentation.ubuntu.com/multipass/latest/how-to-guides/customise-multipass/set-up-the-driver/)).
- Licensing is the sharpest issue for a *product* aimed at corporate machines: the VirtualBox base package is GPLv2
  and free, but the **Extension Pack's Personal Use and Educational License (PUEL) explicitly excludes any commercial,
  business, governmental, or organizational use** beyond a 30-day evaluation; ongoing organizational use requires
  Oracle's paid Enterprise license ([VirtualBox Licensing FAQ](https://www.virtualbox.org/wiki/Licensing_FAQ), [VirtualBox PUEL](https://www.virtualbox.org/wiki/VirtualBox_PUEL)). The Extension Pack gates USB 2.0/3.0
  support, RDP, PXE boot ROM, and disk encryption — features that likely matter for talos-box's VM story.

## Corporate-fleet availability, generally

- All hypervisor-feature-based options (Hyper-V, WHPX, Virtual Machine Platform/WSL2) require **local Administrator,
  one time, to enable the Windows optional feature**, typically followed by a reboot. None of the surveyed precedent
  projects claim a way around this first-run elevation.
- IT departments can and do lock this down further: Microsoft documents disabling Hyper-V/virtualization features
  fleet-wide via Group Policy System Services, and blocking the underlying Windows Features UI or PowerShell/DISM
  paths via GPO/Intune on managed endpoints
  ([Microsoft: block virtualization features on specific computers](https://learn.microsoft.com/en-us/troubleshoot/windows-server/virtualization/block-users-from-running-virtualization-features-on-specific-computers)).
  Whether *this specific* target fleet (Windows 11 Pro/Enterprise, corporate-managed) has such restrictions is
  unknown and should be verified directly rather than assumed **[open question]**.
- Virtualization-Based Security (VBS)/Credential Guard conflicts are a real, widely-documented problem for classic
  type-2 hypervisors (VMware Workstation, VirtualBox) that need exclusive access to VT-x/AMD-V — but Hyper-V, WHPX,
  and WSL2's Virtual Machine Platform all share the *same* underlying hypervisor as VBS, so they should not conflict
  with each other the way VirtualBox does **[inference — not directly confirmed against a VBS-enabled corporate Windows 11 box in this research]**.
- If talos-box's Windows component runs as a Windows service (LocalSystem), the *runtime* privilege question for
  Hyper-V/WMI/PowerShell largely disappears — LocalSystem already has effectively-administrative rights locally. The
  privilege question that actually matters is narrower: (1) can the *installer* get one-time admin to enable the
  Hyper-V/WHPX/Virtual-Machine-Platform feature, and (2) does corporate policy allow installing an admin-run Windows
  service at all. This reframes several of the "privilege at runtime" cells above **[inference; not verified against talos-box's actual Windows service design, which appears undecided at time of writing]**.

## Open questions

1. Does the actual target corporate fleet (whatever org talos-box is being built for) block Hyper-V/WHPX/Virtual
   Machine Platform via GPO or Intune, or is it merely off-by-default? This changes whether "requires one-time admin"
   is a minor UX step or a hard blocker requiring a help-desk ticket per machine.
2. Will talos-box's Windows agent run as a privileged Windows service (LocalSystem) or as the logged-in user? This
   determines whether the Hyper-V-Administrators-group/non-admin WMI path is worth pursuing at all, or whether plain
   PowerShell-as-admin is sufficient.
3. Is there a real appetite for shipping/depending on the VirtualBox Extension Pack, given the PUEL commercial-use
   restriction and the procurement friction of an Oracle Enterprise license?
4. Would a raw-HCS-JSON-schema approach (bypassing hcsshim's `internal/uvm` restriction) be worth prototyping, given
   it's the only in-box path that doesn't touch the classic Hyper-V VM object model at all (no `.vmcx`/Hyper-V Manager
   visibility) — or is that added opacity a downside rather than an upside for an ops-facing tool?
5. Does QEMU+WHPX actually need admin/elevated rights at VM-run time on a locked-down corporate box, or only at
   feature-enablement time? Not verified in this research — worth a hands-on test.
6. For code-reuse with talos-box's existing (presumably QEMU-based, given the repo's history of snapshot/suspend/BGP
   features) Linux/macOS backends: how much of the existing QEMU driver code could be shared with a WHPX-accelerated
   Windows backend versus how much is platform-specific glue?

## Sources

- [Msvm_VirtualSystemManagementService class — Microsoft Learn](https://learn.microsoft.com/en-us/windows/win32/hyperv_v2/msvm-virtualsystemmanagementservice)
- [microsoft/wmi (GitHub)](https://github.com/microsoft/wmi)
- [gabriel-samfira/go-wmi (GitHub)](https://github.com/gabriel-samfira/go-wmi)
- [minikube hyperv driver docs](https://minikube.sigs.k8s.io/docs/drivers/hyperv/)
- [minikube issue #2634 — Hyper-V PowerShell Module is not available](https://github.com/kubernetes/minikube/issues/2634)
- [minikube issue #6104 — New-VM VirtualizationException](https://github.com/kubernetes/minikube/issues/6104)
- [IBM docs — adding non-admin user to Hyper-V Administrators](https://www.ibm.com/docs/en/capm?topic=cmhvm-adding-non-administrator-user-in-hyper-v-administrator-users-group)
- [QEMU: Windows Hypervisor Platform (WHPX) accelerator docs](https://www.qemu.org/docs/master/system/whpx.html)
- [Microsoft/hcsshim (GitHub)](https://github.com/microsoft/hcsshim)
- [Microsoft/hcsshim (pkg.go.dev)](https://pkg.go.dev/github.com/Microsoft/hcsshim)
- [WSL "About" docs — Microsoft Learn](https://learn.microsoft.com/en-us/windows/wsl/about)
- [kind: Using WSL2](https://kind.sigs.k8s.io/docs/user/using-wsl2/)
- [Podman machine docs (docs.podman.io)](https://docs.podman.io/en/latest/markdown/podman-machine.1.html)
- [Multipass: how to set up the driver](https://documentation.ubuntu.com/multipass/latest/how-to-guides/customise-multipass/set-up-the-driver/)
- [VirtualBox Licensing FAQ](https://www.virtualbox.org/wiki/Licensing_FAQ)
- [VirtualBox PUEL](https://www.virtualbox.org/wiki/VirtualBox_PUEL)
- [Microsoft: How to block users from running virtualization features on specific computers](https://learn.microsoft.com/en-us/troubleshoot/windows-server/virtualization/block-users-from-running-virtualization-features-on-specific-computers)
