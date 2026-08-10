//go:build darwin

package helper

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/randax/talos-box/internal/domain"
	"github.com/randax/talos-box/internal/resolverset"
)

func installHostResolver(port int) error {
	if err := installResolver(resolverPath, port); err != nil {
		return err
	}
	return hupMDNSResponder()
}

// hupMDNSResponder makes resolver-file changes take effect immediately;
// mDNSResponder's own pickup timing is undocumented.
func hupMDNSResponder() error {
	if err := exec.Command("/usr/bin/killall", "-HUP", "mDNSResponder").Run(); err != nil {
		return fmt.Errorf("signal mDNSResponder: %w", err)
	}
	return nil
}

// syncDomainResolvers converges /etc/resolver/<domain> files for clusters
// with custom domains. The helper's privilege boundary is a state-derived
// allow-list (SPEC §11): every requested name must be a valid canonical
// domain, and only files carrying the ownership marker are ever removed. The
// shared default-suffix file is not managed here.
func syncDomainResolvers(domains []string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("DNS port %d is outside 1..65535", port)
	}
	// Connections are served concurrently; observe-plan-mutate must not
	// interleave with itself.
	resolverSyncMu.Lock()
	defer resolverSyncMu.Unlock()
	wanted := make([]string, 0, len(domains))
	for _, name := range domains {
		canonical, err := domain.Validate(name, true)
		if err != nil {
			return fmt.Errorf("refuse resolver sync: %w", err)
		}
		if canonical != name {
			return fmt.Errorf("refuse resolver sync: domain %q is not canonical", name)
		}
		wanted = append(wanted, canonical)
	}

	directory := filepath.Dir(resolverPath)
	observed := map[string][]byte{}
	entries, err := os.ReadDir(directory)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read resolver directory: %w", err)
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		content, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return fmt.Errorf("read resolver file %s: %w", entry.Name(), err)
		}
		observed[entry.Name()] = content
	}

	create, remove := resolverset.Plan(wanted, observed, port)
	for _, name := range create {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return fmt.Errorf("create resolver directory: %w", err)
		}
		if err := os.WriteFile(filepath.Join(directory, name), []byte(resolverset.Content(port)), 0o644); err != nil {
			return fmt.Errorf("write resolver file %s: %w", name, err)
		}
	}
	for _, name := range remove {
		if err := os.Remove(filepath.Join(directory, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove resolver file %s: %w", name, err)
		}
	}
	if len(create) != 0 || len(remove) != 0 || pendingHUP {
		if err := hupMDNSResponder(); err != nil {
			// Files converged but the reload didn't land; remember, or the
			// next converged sync would never retry it.
			pendingHUP = true
			return err
		}
		pendingHUP = false
	}
	return nil
}

// pendingHUP records a failed mDNSResponder reload so a later sync retries
// it even when the file set is already converged. Guarded by resolverSyncMu.
var (
	resolverSyncMu sync.Mutex
	pendingHUP     bool
)

func uninstallHostResolver() error {
	err := os.Remove(resolverPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove resolver file: %w", err)
	}
	return hupMDNSResponder()
}
