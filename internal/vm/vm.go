package vm

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Code-Hex/vz/v3"
)

const gracefulStopTimeout = 30 * time.Second

// Config contains the host resources backing one virtual machine.
type Config struct {
	CPUs              int
	MemoryMiB         int
	DiskPath          string
	MAC               string
	NetworkFile       *os.File
	NetworkCleanup    func() error
	EFIVarsPath       string
	ConsoleSocketPath string
}

// VM owns a Virtualization.framework VM and its reconnectable console socket.
type VM struct {
	machine        *vz.VirtualMachine
	console        *consoleProxy
	serialFiles    [2]*os.File
	networkFile    *os.File
	networkCleanup func() error
	closeMu        sync.Mutex
	closed         bool
}

// New translates config to the Virtualization.framework devices required by Talos.
func New(config Config) (*VM, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}

	bootLoader, err := newEFIBootLoader(config.EFIVarsPath)
	if err != nil {
		return nil, err
	}
	machineConfig, err := vz.NewVirtualMachineConfiguration(
		bootLoader,
		uint(config.CPUs),
		uint64(config.MemoryMiB)*1024*1024,
	)
	if err != nil {
		return nil, fmt.Errorf("create VM configuration: %w", err)
	}

	proxy, guestRead, guestWrite, err := newConsoleProxy(config.ConsoleSocketPath)
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

	if err := configureDevices(machineConfig, config, guestRead, guestWrite); err != nil {
		return nil, err
	}
	valid, err := machineConfig.Validate()
	if err != nil {
		return nil, fmt.Errorf("validate VM configuration: %w", err)
	}
	if !valid {
		return nil, errors.New("VM configuration is invalid")
	}

	machine, err := vz.NewVirtualMachine(machineConfig)
	if err != nil {
		return nil, fmt.Errorf("create VM: %w", err)
	}
	configured = true
	return &VM{
		machine:        machine,
		console:        proxy,
		serialFiles:    [2]*os.File{guestRead, guestWrite},
		networkFile:    config.NetworkFile,
		networkCleanup: config.NetworkCleanup,
	}, nil
}

func validateConfig(config Config) error {
	switch {
	case config.CPUs <= 0:
		return errors.New("CPUs must be greater than zero")
	case config.MemoryMiB <= 0:
		return errors.New("memory must be greater than zero")
	case config.DiskPath == "":
		return errors.New("disk path is required")
	case config.MAC == "":
		return errors.New("MAC address is required")
	case config.NetworkFile == nil:
		return errors.New("network file is required")
	case config.NetworkCleanup == nil:
		return errors.New("network cleanup is required")
	case config.EFIVarsPath == "":
		return errors.New("EFI variable store path is required")
	case config.ConsoleSocketPath == "":
		return errors.New("console socket path is required")
	default:
		return nil
	}
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

func configureDevices(machineConfig *vz.VirtualMachineConfiguration, config Config, guestRead, guestWrite *os.File) error {
	diskAttachment, err := vz.NewDiskImageStorageDeviceAttachment(config.DiskPath, false)
	if err != nil {
		return fmt.Errorf("attach disk image: %w", err)
	}
	disk, err := vz.NewVirtioBlockDeviceConfiguration(diskAttachment)
	if err != nil {
		return fmt.Errorf("create virtio block device: %w", err)
	}
	machineConfig.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{disk})

	attachment, err := vz.NewFileHandleNetworkDeviceAttachment(config.NetworkFile)
	if err != nil {
		return fmt.Errorf("create file-handle network attachment: %w", err)
	}
	networkDevice, err := vz.NewVirtioNetworkDeviceConfiguration(attachment)
	if err != nil {
		return fmt.Errorf("create virtio network device: %w", err)
	}
	hardwareAddr, err := net.ParseMAC(config.MAC)
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

// Start starts the virtual machine.
func (v *VM) Start() error {
	return v.machine.Start()
}

// Suspend pauses the running VM and saves its full state (RAM + devices) to
// savePath (vz save/restore, macOS 14+). The VM is left paused; the caller
// closes it afterward.
func (v *VM) Suspend(savePath string) error {
	if err := v.machine.Pause(); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	if err := v.machine.SaveMachineStateToPath(savePath); err != nil {
		return fmt.Errorf("save machine state: %w", err)
	}
	return nil
}

// RestoreState restores a freshly-created (Stopped) VM from a saved state file
// and resumes it. NOTE: vz RestoreMachineStateFromURL fails with "invalid
// argument" against talosbox's device set (fresh console/serial handles on
// recreation), so in practice resume falls back to a cold boot — see #37.
func (v *VM) RestoreState(savePath string) error {
	if err := v.machine.RestoreMachineStateFromURL(savePath); err != nil {
		return fmt.Errorf("restore machine state: %w", err)
	}
	if err := v.machine.Resume(); err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	return nil
}

// Stop asks the guest to shut down, then forcibly stops it after a timeout.
func (v *VM) Stop() error {
	state := v.State()
	if state == vz.VirtualMachineStateStopped || state == vz.VirtualMachineStateError {
		return nil
	}

	var requested bool
	var requestErr error
	if v.machine.CanRequestStop() {
		requested, requestErr = v.machine.RequestStop()
	}
	if requested {
		timer := time.NewTimer(gracefulStopTimeout)
		defer timer.Stop()
		for v.State() != vz.VirtualMachineStateStopped {
			select {
			case state := <-v.machine.StateChangedNotify():
				if state == vz.VirtualMachineStateStopped {
					return nil
				}
			case <-timer.C:
				return v.forceStop(requestErr)
			}
		}
		return nil
	}

	return v.forceStop(requestErr)
}

// Close stops the VM and releases its console and serial resources.
func (v *VM) Close() error {
	v.closeMu.Lock()
	defer v.closeMu.Unlock()
	if v.closed {
		return nil
	}
	if err := v.Stop(); err != nil {
		return err
	}
	v.console.close()
	for _, file := range v.serialFiles {
		_ = file.Close()
	}
	var cleanupErr error
	if v.networkFile != nil {
		cleanupErr = v.networkFile.Close()
		v.networkFile = nil
	}
	if v.networkCleanup != nil {
		if err := v.networkCleanup(); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		} else {
			v.networkCleanup = nil
		}
	}
	if cleanupErr != nil {
		return cleanupErr
	}
	v.closed = true
	return nil
}

// Active reports whether the VM is running or in an active transition.
func (v *VM) Active() bool {
	state := v.State()
	return state != vz.VirtualMachineStateStopped && state != vz.VirtualMachineStateError
}

func (v *VM) forceStop(requestErr error) error {
	if v.State() == vz.VirtualMachineStateStopped {
		return nil
	}
	if err := v.machine.Stop(); err != nil {
		return errors.Join(requestErr, fmt.Errorf("force stop VM: %w", err))
	}
	return nil
}

// State returns the current Virtualization.framework state.
func (v *VM) State() vz.VirtualMachineState {
	return v.machine.State()
}
