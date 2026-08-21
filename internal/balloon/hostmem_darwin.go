//go:build darwin

package balloon

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// HostTotalMiB returns physical RAM in MiB.
func HostTotalMiB() (int, error) {
	bytes, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, err
	}
	return int(bytes / 1024 / 1024), nil
}

// HostFreeMiB estimates memory available to the host (free + inactive +
// speculative pages), parsed from vm_stat. This is what tbxd watches for
// pressure — a low value triggers balloon inflation.
func HostFreeMiB() (int, error) {
	return HostFreeMiBContext(context.Background())
}

// HostFreeMiBContext is HostFreeMiB bounded by ctx. Callers on a foreground
// path (doctor, cluster create) pass a deadline: vm_stat is a subprocess and a
// stalled one would otherwise hang the command with no exit code.
func HostFreeMiBContext(ctx context.Context) (int, error) {
	out, err := exec.CommandContext(ctx, "vm_stat").Output()
	if err != nil {
		if ctx.Err() != nil {
			return 0, fmt.Errorf("vm_stat: %w", ctx.Err())
		}
		return 0, err
	}
	pageSize := 4096
	pages := map[string]int{}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "page size of") {
			if n := extractInt(line); n > 0 {
				pageSize = n
			}
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		pages[strings.TrimSpace(parts[0])] = extractInt(parts[1])
	}
	freePages := pages["Pages free"] + pages["Pages inactive"] + pages["Pages speculative"]
	return freePages * pageSize / 1024 / 1024, nil
}

func extractInt(s string) int {
	var digits strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	n, _ := strconv.Atoi(digits.String())
	return n
}
