//go:build darwin && arm64

package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/Code-Hex/vz/v3"
	"github.com/randax/talos-box/internal/helper"
)

type vzHypervisor struct {
	capabilities Capabilities

	savedMu sync.Mutex
	saved   map[string]*vzMachine
}

// New probes the host Virtualization.framework backend once.
func New(ctx context.Context) (Hypervisor, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("probe Virtualization.framework: %w", err)
	}
	if vz.VirtualMachineConfigurationMinimumAllowedCPUCount() == 0 ||
		vz.VirtualMachineConfigurationMinimumAllowedMemorySize() == 0 {
		return nil, errors.New("virtualization.framework reported no usable VM configuration")
	}
	bootLoader, err := vz.NewEFIBootLoader()
	if err != nil {
		return nil, fmt.Errorf("probe Virtualization.framework EFI support: %w", err)
	}
	probe, err := vz.NewVirtualMachineConfiguration(
		bootLoader,
		vz.VirtualMachineConfigurationMinimumAllowedCPUCount(),
		vz.VirtualMachineConfigurationMinimumAllowedMemorySize(),
	)
	if err != nil {
		return nil, fmt.Errorf("probe Virtualization.framework configuration: %w", err)
	}
	valid, err := probe.Validate()
	if err != nil {
		return nil, fmt.Errorf("probe Virtualization.framework (verify the virtualization entitlement and host support): %w", err)
	}
	if !valid {
		return nil, errors.New("virtualization.framework is unusable; verify the virtualization entitlement and host support")
	}
	return &vzHypervisor{
		capabilities: Capabilities{
			Suspend: suspendFeatureStatus(),
			BalloonReadback: FeatureStatus{
				Reason: "Virtualization.framework does not report the guest-visible balloon size",
			},
		},
		saved: make(map[string]*vzMachine),
	}, nil
}

func (h *vzHypervisor) Capabilities() Capabilities { return h.capabilities }

func (h *vzHypervisor) Architecture() Architecture { return ArchitectureARM64 }

// Launch transactionally constructs and starts or restores a VZ machine.
func (h *vzHypervisor) Launch(ctx context.Context, spec Spec) (Machine, error) {
	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("launch VM: %w", err)
	}

	var machine *vzMachine
	if spec.Restore != nil && spec.Restore.Path != "" {
		machine = h.takeSaved(spec.Restore.Path)
	}
	if machine == nil {
		var err error
		machine, err = h.newMachine(spec)
		if err != nil {
			return nil, err
		}
	}

	started := false
	defer func() {
		if !started {
			_ = machine.Close()
		}
	}()

	if spec.Restore != nil && spec.Restore.Path != "" {
		if err := h.restore(ctx, machine, *spec.Restore); err == nil {
			started = true
			return machine, nil
		} else if spec.Restore.Fallback != nil {
			spec.Restore.Fallback(err)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("launch VM: %w", err)
	}
	if err := machine.machine.Start(); err != nil {
		return nil, fmt.Errorf("start VM: %w", err)
	}
	started = true
	return machine, nil
}

func (h *vzHypervisor) newMachine(spec Spec) (*vzMachine, error) {
	if spec.Network == nil {
		return nil, errors.New("network attachment provider is required")
	}
	attachment, err := spec.Network()
	if err != nil {
		return nil, fmt.Errorf("attach network: %w", err)
	}
	if attachment == nil {
		return nil, errors.New("network attachment provider returned nil")
	}
	owned := false
	defer func() {
		if !owned {
			_ = attachment.Close()
		}
	}()
	if attachment.Kind != helper.AttachmentDatagramFD {
		return nil, fmt.Errorf("%w: VZ requires %q network attachment, got %q", ErrUnsupported, helper.AttachmentDatagramFD, attachment.Kind)
	}
	if attachment.File == nil {
		return nil, errors.New("network attachment has no descriptor")
	}

	bootLoader, err := newEFIBootLoader(spec.EFIVarsPath)
	if err != nil {
		return nil, err
	}
	machineConfig, err := vz.NewVirtualMachineConfiguration(
		bootLoader,
		uint(spec.CPUs),
		uint64(spec.MemoryMiB)*1024*1024,
	)
	if err != nil {
		return nil, fmt.Errorf("create VM configuration: %w", err)
	}

	proxy, guestRead, guestWrite, err := newConsoleProxy(spec.ConsoleSocketPath)
	if err != nil {
		return nil, err
	}
	configured := false
	defer func() {
		if !configured {
			_ = guestRead.Close()
			_ = guestWrite.Close()
			proxy.close()
		}
	}()

	if err := configureVZDevices(machineConfig, spec, attachment.File, guestRead, guestWrite); err != nil {
		return nil, err
	}
	valid, err := machineConfig.Validate()
	if err != nil {
		return nil, fmt.Errorf("validate VM configuration: %w", err)
	}
	if !valid {
		return nil, errors.New("VM configuration is invalid")
	}

	virtualMachine, err := vz.NewVirtualMachine(machineConfig)
	if err != nil {
		return nil, fmt.Errorf("create VM: %w", err)
	}
	// MemoryBalloonDevices mints a fresh finalized wrapper on every call. Keep
	// exactly one wrapper to avoid over-releasing the Objective-C object (#38).
	var balloon *vz.VirtioTraditionalMemoryBalloonDevice
	if devices := virtualMachine.MemoryBalloonDevices(); len(devices) > 0 {
		balloon, _ = devices[0].(*vz.VirtioTraditionalMemoryBalloonDevice)
	}
	configured = true
	owned = true
	return &vzMachine{
		owner:       h,
		machine:     virtualMachine,
		console:     proxy,
		serialFiles: [2]*os.File{guestRead, guestWrite},
		network:     attachment,
		balloon:     balloon,
	}, nil
}

func newEFIBootLoader(path string) (*vz.EFIBootLoader, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create EFI variable store directory: %w", err)
	}
	var options []vz.NewEFIVariableStoreOption
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		options = append(options, vz.WithCreatingEFIVariableStore())
	} else if err != nil {
		return nil, fmt.Errorf("inspect EFI variable store: %w", err)
	}
	store, err := vz.NewEFIVariableStore(path, options...)
	if err != nil {
		return nil, fmt.Errorf("open EFI variable store: %w", err)
	}
	bootLoader, err := vz.NewEFIBootLoader(vz.WithEFIVariableStore(store))
	if err != nil {
		return nil, fmt.Errorf("create EFI boot loader: %w", err)
	}
	return bootLoader, nil
}

func configureVZDevices(machineConfig *vz.VirtualMachineConfiguration, spec Spec, networkFile, guestRead, guestWrite *os.File) error {
	diskAttachment, err := vz.NewDiskImageStorageDeviceAttachment(spec.DiskPath, false)
	if err != nil {
		return fmt.Errorf("attach disk image: %w", err)
	}
	disk, err := vz.NewVirtioBlockDeviceConfiguration(diskAttachment)
	if err != nil {
		return fmt.Errorf("create virtio block device: %w", err)
	}
	machineConfig.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{disk})

	attachment, err := vz.NewFileHandleNetworkDeviceAttachment(networkFile)
	if err != nil {
		return fmt.Errorf("create file-handle network attachment: %w", err)
	}
	networkDevice, err := vz.NewVirtioNetworkDeviceConfiguration(attachment)
	if err != nil {
		return fmt.Errorf("create virtio network device: %w", err)
	}
	hardwareAddr, err := net.ParseMAC(spec.MAC)
	if err != nil {
		return fmt.Errorf("parse MAC address: %w", err)
	}
	mac, err := vz.NewMACAddress(hardwareAddr)
	if err != nil {
		return fmt.Errorf("create VZ MAC address: %w", err)
	}
	networkDevice.SetMACAddress(mac)
	machineConfig.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{networkDevice})

	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return fmt.Errorf("create virtio entropy device: %w", err)
	}
	machineConfig.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})

	balloon, err := vz.NewVirtioTraditionalMemoryBalloonDeviceConfiguration()
	if err != nil {
		return fmt.Errorf("create memory balloon device: %w", err)
	}
	machineConfig.SetMemoryBalloonDevicesVirtualMachineConfiguration([]vz.MemoryBalloonDeviceConfiguration{balloon})

	serialAttachment, err := vz.NewFileHandleSerialPortAttachment(guestRead, guestWrite)
	if err != nil {
		return fmt.Errorf("create serial attachment: %w", err)
	}
	serial, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serialAttachment)
	if err != nil {
		return fmt.Errorf("create virtio serial port: %w", err)
	}
	machineConfig.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{serial})
	return nil
}

func (h *vzHypervisor) restore(ctx context.Context, machine *vzMachine, restore Restore) error {
	if !h.capabilities.Suspend.Supported {
		return fmt.Errorf("%w: %s", ErrUnsupported, h.capabilities.Suspend.Reason)
	}
	if _, err := os.Stat(restore.Path); err != nil {
		return fmt.Errorf("%w: %v", ErrIncompatibleSave, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := restoreVZMachine(machine.machine, restore.Path); err != nil {
		return fmt.Errorf("%w: %v", ErrIncompatibleSave, err)
	}
	if err := ctx.Err(); err != nil {
		_ = machine.forceStop(nil)
		return err
	}
	if err := machine.machine.Resume(); err != nil {
		_ = machine.forceStop(nil)
		return fmt.Errorf("%w: resume restored VM: %v", ErrIncompatibleSave, err)
	}
	return nil
}

func (h *vzHypervisor) retain(path string, machine *vzMachine) error {
	h.savedMu.Lock()
	defer h.savedMu.Unlock()
	if existing := h.saved[path]; existing != nil && existing != machine {
		return fmt.Errorf("saved state %q is already owned by another VM", path)
	}
	h.saved[path] = machine
	return nil
}

func (h *vzHypervisor) takeSaved(path string) *vzMachine {
	h.savedMu.Lock()
	defer h.savedMu.Unlock()
	machine := h.saved[path]
	delete(h.saved, path)
	return machine
}

func (h *vzHypervisor) forget(machine *vzMachine) {
	h.savedMu.Lock()
	defer h.savedMu.Unlock()
	for path, saved := range h.saved {
		if saved == machine {
			delete(h.saved, path)
		}
	}
}

type vzMachine struct {
	owner       *vzHypervisor
	machine     *vz.VirtualMachine
	console     *consoleProxy
	serialFiles [2]*os.File
	network     *helper.Attachment
	balloon     *vz.VirtioTraditionalMemoryBalloonDevice

	closeMu sync.Mutex
	closed  bool
}

func (v *vzMachine) Active() bool {
	state := v.machine.State()
	return state != vz.VirtualMachineStateStopped && state != vz.VirtualMachineStateError
}

func (v *vzMachine) SetMemoryTargetMiB(targetMiB int) error {
	if v.machine.State() != vz.VirtualMachineStateRunning {
		return ErrDeviceNotActive
	}
	if v.balloon == nil {
		return fmt.Errorf("%w: no memory balloon device", ErrUnsupported)
	}
	v.balloon.SetTargetVirtualMachineMemorySize(uint64(targetMiB) * 1024 * 1024)
	return nil
}

func (v *vzMachine) Stop(ctx context.Context) error {
	state := v.machine.State()
	if state == vz.VirtualMachineStateStopped || state == vz.VirtualMachineStateError {
		return nil
	}

	var requested bool
	var requestErr error
	if v.machine.CanRequestStop() {
		requested, requestErr = v.machine.RequestStop()
	}
	if requested {
		for v.machine.State() != vz.VirtualMachineStateStopped {
			select {
			case state := <-v.machine.StateChangedNotify():
				if state == vz.VirtualMachineStateStopped {
					return nil
				}
			case <-ctx.Done():
				return v.forceStop(requestErr)
			}
		}
		return nil
	}
	return v.forceStop(requestErr)
}

func (v *vzMachine) Suspend(ctx context.Context, savePath string) error {
	if !v.owner.capabilities.Suspend.Supported {
		return fmt.Errorf("%w: %s", ErrUnsupported, v.owner.capabilities.Suspend.Reason)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if v.machine.State() != vz.VirtualMachineStateRunning {
		return ErrDeviceNotActive
	}
	if err := v.machine.Pause(); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := saveVZMachine(v.machine, savePath); err != nil {
		return fmt.Errorf("save machine state: %w", err)
	}
	if err := v.owner.retain(savePath, v); err != nil {
		return err
	}
	return nil
}

func (v *vzMachine) Close() error {
	v.closeMu.Lock()
	defer v.closeMu.Unlock()
	if v.closed {
		return nil
	}
	if err := v.forceStop(nil); err != nil {
		return err
	}
	v.owner.forget(v)
	v.console.close()
	for _, file := range v.serialFiles {
		_ = file.Close()
	}
	if err := v.network.Close(); err != nil {
		v.closed = true
		return err
	}
	v.closed = true
	return nil
}

func (v *vzMachine) forceStop(prior error) error {
	state := v.machine.State()
	if state == vz.VirtualMachineStateStopped || state == vz.VirtualMachineStateError {
		return nil
	}
	if err := v.machine.Stop(); err != nil {
		return errors.Join(prior, fmt.Errorf("force stop VM: %w", err))
	}
	return nil
}
