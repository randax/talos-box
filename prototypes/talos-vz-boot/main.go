// PROTOTYPE — throwaway code answering wayfinder ticket #11:
// does Talos arm64 boot under Apple Virtualization.framework?
//
//	go run . efi    <iso>                 — EFI boot from metal-arm64 ISO (talos#11865 repro)
//	go run . kernel <kernel> <initramfs>  — VZLinuxBootLoader direct boot (hypothesis: no EFI, no hang)
//
// Success criterion for both modes: VM takes a DHCP lease on the NAT network and
// opens TCP 50000 (apid in maintenance mode). Serial console (hvc0) is streamed to
// stdout in kernel mode; EFI mode is headless (Talos ISO logs to ttyAMA0/framebuffer,
// which VZ does not provide).
package main

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"time"

	"github.com/Code-Hex/vz/v3"
)

// fixed MAC so we can find our lease in /var/db/dhcpd_leases
const macAddr = "52:54:00:aa:bb:05"

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: talos-vz-boot efi <iso> | kernel <kernel> <initramfs>")
		os.Exit(2)
	}
	mode := os.Args[1]

	var bootLoader vz.BootLoader
	var err error
	switch mode {
	case "efi":
		variableStore, verr := vz.NewEFIVariableStore("efi-vars.fd", vz.WithCreatingEFIVariableStore())
		must(verr)
		bootLoader, err = vz.NewEFIBootLoader(vz.WithEFIVariableStore(variableStore))
	case "kernel":
		bootLoader, err = vz.NewLinuxBootLoader(os.Args[2],
			vz.WithCommandLine("init_on_alloc=1 slab_nomerge pti=on consoleblank=0 printk.devkmsg=on talos.platform=metal console=hvc0"),
			vz.WithInitrd(os.Args[3]),
		)
	default:
		fmt.Println("unknown mode", mode)
		os.Exit(2)
	}
	must(err)

	config, err := vz.NewVirtualMachineConfiguration(bootLoader, 2, 3*1024*1024*1024)
	must(err)

	// serial console (hvc0) -> stdout
	serialAttachment, err := vz.NewFileHandleSerialPortAttachment(os.Stdin, os.Stdout)
	must(err)
	consoleConfig, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serialAttachment)
	must(err)
	config.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{consoleConfig})

	// virtio-rng: present in both modes, so the only variable is the boot path
	entropyConfig, err := vz.NewVirtioEntropyDeviceConfiguration()
	must(err)
	config.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropyConfig})

	// NAT network with fixed MAC
	natAttachment, err := vz.NewNATNetworkDeviceAttachment()
	must(err)
	networkConfig, err := vz.NewVirtioNetworkDeviceConfiguration(natAttachment)
	must(err)
	hw, err := net.ParseMAC(macAddr)
	must(err)
	mac, err := vz.NewMACAddress(hw)
	must(err)
	networkConfig.SetMACAddress(mac)
	config.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{networkConfig})

	// EFI mode boots the ISO as a virtio-blk device
	if mode == "efi" {
		isoAttachment, err := vz.NewDiskImageStorageDeviceAttachment(os.Args[2], true)
		must(err)
		iso, err := vz.NewVirtioBlockDeviceConfiguration(isoAttachment)
		must(err)
		config.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{iso})
	}

	validated, err := config.Validate()
	must(err)
	if !validated {
		fmt.Println("config did not validate")
		os.Exit(1)
	}

	vm, err := vz.NewVirtualMachine(config)
	must(err)
	must(vm.Start())
	fmt.Fprintf(os.Stderr, "[prototype] VM started in %s mode, mac=%s; polling dhcpd_leases + tcp/50000\n", mode, macAddr)

	deadline := time.After(5 * time.Minute)
	tick := time.NewTicker(5 * time.Second)
	for {
		select {
		case <-deadline:
			fmt.Fprintf(os.Stderr, "\n[prototype] RESULT=TIMEOUT no apid on :50000 within 5m (mode=%s, ip=%q)\n", mode, leaseIP())
			os.Exit(3)
		case <-tick.C:
			ip := leaseIP()
			if ip == "" {
				continue
			}
			conn, err := net.DialTimeout("tcp", ip+":50000", 2*time.Second)
			if err == nil {
				conn.Close()
				fmt.Fprintf(os.Stderr, "\n[prototype] RESULT=SUCCESS apid reachable at %s:50000 (mode=%s)\n", ip, mode)
				if os.Getenv("KEEP_ALIVE") != "" {
					fmt.Fprintln(os.Stderr, "[prototype] KEEP_ALIVE set: holding VM for 120s for talosctl inspection")
					time.Sleep(120 * time.Second)
				}
				os.Exit(0)
			}
		}
	}
}

func leaseIP() string {
	data, err := os.ReadFile("/var/db/dhcpd_leases")
	if err != nil {
		return ""
	}
	// leases store the MAC without leading zeros
	re := regexp.MustCompile(`ip_address=(\S+)\n\thw_address=1,52:54:0:aa:bb:5`)
	m := re.FindStringSubmatch(string(data))
	if m == nil {
		return ""
	}
	return m[1]
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
