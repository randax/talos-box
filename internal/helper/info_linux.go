//go:build linux

package helper

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const requiredLinuxCapabilityMask uint64 = 1<<10 | 1<<12 | 1<<13

var readProcStatus = os.ReadFile

func currentHelperInfo() (Info, int, func(), error) {
	capabilities, err := currentEffectiveCapabilities()
	if err != nil {
		return Info{}, -1, nil, fmt.Errorf("read helper capabilities: %w", err)
	}
	return Info{
		ProtocolVersion:          protocolVersion,
		EffectiveCapabilities:    capabilities,
		EffectiveCapabilityNames: capabilityNames(capabilities),
	}, -1, nil, nil
}

func currentEffectiveCapabilities() (uint64, error) {
	status, err := readProcStatus("/proc/self/status")
	if err != nil {
		return 0, err
	}
	return parseEffectiveCapabilities(status)
}

func parseEffectiveCapabilities(status []byte) (uint64, error) {
	for _, line := range strings.Split(string(status), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok || name != "CapEff" {
			continue
		}
		return strconv.ParseUint(strings.TrimSpace(value), 16, 64)
	}
	return 0, fmt.Errorf("CapEff not found in proc status")
}

func capabilityNames(mask uint64) []string {
	capabilities := []struct {
		bit  uint
		name string
	}{
		{bit: 10, name: "CAP_NET_BIND_SERVICE"},
		{bit: 12, name: "CAP_NET_ADMIN"},
		{bit: 13, name: "CAP_NET_RAW"},
	}
	names := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		if mask&(1<<capability.bit) != 0 {
			names = append(names, capability.name)
		}
	}
	return names
}
