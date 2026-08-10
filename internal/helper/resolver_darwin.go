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

// pendingHUP records a failed or still-owed mDNSResponder reload so a later
// sync retries it even when the file set is already converged. Guarded by
// resolverSyncMu.
var (
	resolverSyncMu sync.Mutex
	pendingHUP     bool
)

func installHostResolver(port int) error {
	resolverSyncMu.Lock()
	defer resolverSyncMu.Unlock()
	if err := installResolver(resolverPath, port); err != nil {
		return err
	}
	return reloadResolverCache()
}

func uninstallHostResolver() error {
	resolverSyncMu.Lock()
	defer resolverSyncMu.Unlock()
	err := os.Remove(resolverPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove resolver file: %w", err)
	}
	return reloadResolverCache()
}

// reloadResolverCache HUPs mDNSResponder (its pickup of resolver-file changes
// is undocumented) and tracks the debt in pendingHUP: set before the attempt,
// cleared only on success, so partial syncs and failed reloads are retried by
// a later call even when the file set is already converged. Callers hold
// resolverSyncMu.
func reloadResolverCache() error {
	pendingHUP = true
	if err := exec.Command("/usr/bin/killall", "-HUP", "mDNSResponder").Run(); err != nil {
		return fmt.Errorf("signal mDNSResponder: %w", err)
	}
	pendingHUP = false
	return nil
}

// syncDomainResolvers converges /etc/resolver/<domain> files for clusters
// with custom domains. Every requested name must be a valid canonical domain
// (SPEC §11), only marker-bearing files are ever removed or rewritten, and a
// wanted name occupied by anything unmanaged — including a symlink — is left
// untouched. The shared default-suffix file is not managed here.
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
	occupied := map[string]bool{}
	entries, err := os.ReadDir(directory)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read resolver directory: %w", err)
	}
	for _, entry := range entries {
		occupied[entry.Name()] = true
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
	if len(create) != 0 || len(remove) != 0 {
		// Owe a reload before mutating, so a partial failure below still
		// gets its HUP retried on the next sync.
		pendingHUP = true
	}
	for _, name := range create {
		if occupied[name] {
			// Present but not a regular file (e.g. a symlink): writing would
			// follow it as root. Not ours; report, never touch.
			return fmt.Errorf("resolver path %s exists but is not a managed regular file; remove it manually", filepath.Join(directory, name))
		}
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
	if pendingHUP {
		return reloadResolverCache()
	}
	return nil
}
