package hypervisor

import "errors"

func validateSpec(spec Spec) error {
	switch {
	case spec.CPUs <= 0:
		return errors.New("CPUs must be greater than zero")
	case spec.MemoryMiB <= 0:
		return errors.New("memory must be greater than zero")
	case spec.DiskPath == "":
		return errors.New("disk path is required")
	case spec.MAC == "":
		return errors.New("MAC address is required")
	case spec.EFIVarsPath == "":
		return errors.New("EFI variable store path is required")
	case spec.ConsoleSocketPath == "":
		return errors.New("console socket path is required")
	default:
		return nil
	}
}
