# Guest memory and reclaim

talosbox's curated provisioning path adds conservative reclaim settings to each generated Talos
machine config. Substrate-only clusters bring their own machine config, so apply the equivalent
patch yourself if you want the same policy.

## Reclaim defaults

`vm.min_free_kbytes` is calculated independently for each node:

```text
memory in MiB × 32 KiB, clamped to 16,384–262,144 KiB
```

That gives 16,384 KiB at 512 MiB, 32,768 KiB at 1 GiB, 65,536 KiB at 2 GiB,
131,072 KiB at 4 GiB, and 262,144 KiB at 8 GiB or more.

For a 2 GiB substrate-only node, save this as `guest-memory.yaml`:

```yaml
machine:
  sysctls:
    vm.min_free_kbytes: "65536"
    vm.watermark_scale_factor: "200"
    vm.vfs_cache_pressure: "50"
  kubelet:
    extraConfig:
      evictionHard:
        memory.available: 300Mi
      systemReserved:
        memory: 512Mi
```

Recalculate `vm.min_free_kbytes` for nodes with a different memory size. Merge the document when
generating a new config:

```sh
talosctl gen config demo https://<control-plane-ip>:6443 \
  --config-patch @guest-memory.yaml
```

For an already-configured node, apply it directly:

```sh
talosctl patch mc -p @guest-memory.yaml --nodes <node-ip>
```

The kubelet reservation and eviction threshold reduce memory visible to workloads, which is
material on small guests; the curated path applies them only to nodes with at least 2 GiB. To
keep the reclaim sysctls but leave kubelet schedulable memory unchanged, omit the entire `machine.kubelet` block. On the curated path, set
`kubeletMemoryProtection: false` on the cluster instead; this field requires a curated `cni:`.

## Disable virtio ballooning

Ballooning lets `tbxd` reclaim unused guest memory when the host is under pressure. If a workload
must opt out, create a bring-your-own Image Factory schematic whose kernel arguments are exactly
ordered with both mandatory console arguments first:

```yaml
customization:
  extraKernelArgs:
    - console=tty0
    - console=hvc0
    - module_blacklist=virtio_balloon
```

Use the resulting schematic ID in `talosbox.yaml`:

```yaml
talos:
  schematic: <schematic-id>
```

There is no `talos.disableBalloon` setting. The schematic is the opt-out. Keep any system
extensions your cluster needs in that schematic or in `talos.extensions`.

With `virtio_balloon` blacklisted, `tbxd` loses its guest-memory reclamation mechanism for those
VMs. Their unused memory cannot be returned under host pressure, so expect higher host memory
use and less room for other guests. Kernel arguments are fixed in the boot image: selecting this
option requires the new schematic image and recreating affected nodes; patching machine config
alone cannot change it.

Already-configured clusters are never silently patched. Sysctl and kubelet changes can be applied
with `talosctl patch mc`, but the balloon blacklist takes effect only after booting a newly built
image and recreating the node.

## Verify

Check the live sysctls:

```sh
talosctl get sysctls --nodes <node-ip> | grep -E \
  'vm.min_free_kbytes|vm.watermark_scale_factor|vm.vfs_cache_pressure'
```

Inspect kubelet's effective configuration through the API server:

```sh
kubectl get --raw /api/v1/nodes/<node-name>/proxy/configz
```

For a balloon-disabled image, confirm the boot command line and inspect the serial-console boot
log for the blacklist and any `virtio_balloon` messages:

```sh
talosctl read --nodes <node-ip> /proc/cmdline
tbx console <cluster> <node> --no-follow | grep -E 'virtio_balloon|Kernel command line'
```
