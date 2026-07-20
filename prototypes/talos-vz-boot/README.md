# PROTOTYPE: talos-vz-boot

Throwaway prototype for [wayfinder ticket #11](https://github.com/randax/talos-box/issues/11):
does Talos arm64 boot under Apple Virtualization.framework, or does the EFI entropy hang
(siderolabs/talos#11865) block it?

Not production code. No error handling, no tests, no cleanup.

## Run

```sh
./fetch-assets.sh   # downloads ISO + kernel + initramfs from Image Factory (vanilla schematic, v1.13.6)
./run.sh efi        # EFI boot from the metal-arm64 ISO — the talos#11865 repro
./run.sh kernel     # VZLinuxBootLoader direct kernel+initramfs boot — the hypothesis
```

Success criterion (both modes): the VM takes a DHCP lease and opens TCP 50000
(apid in maintenance mode). `run.sh` builds, ad-hoc codesigns with the
`com.apple.security.virtualization` entitlement, and runs with a 5-minute timeout.
Serial console (hvc0) streams to stdout in kernel mode; EFI mode is headless because
the Talos ISO logs to ttyAMA0/framebuffer, neither of which VZ provides.
