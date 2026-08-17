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
	// Owe the reload before mutating: a write that half-lands (or a chmod
	// failure) must still get its HUP replayed later.
	pendingHUP = true
	if err := installResolver(resolverPath, port); err != nil {
		return err
	}
	return reloadResolverCache()
}

func uninstallHostResolver() error {
	resolverSyncMu.Lock()
	defer resolverSyncMu.Unlock()
	removed := false
	err := os.Remove(resolverPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove resolver file: %w", err)
	}
	if err == nil {
		removed = true
		pendingHUP = true
	}
	// Uninstall is terminal: marker-managed per-domain files (SPEC §11) have
	// no owner after the helper is gone, so sweep them here too. Unmanaged
	// files are never touched.
	directory := filepath.Dir(resolverPath)
	entries, readErr := os.ReadDir(directory)
	var sweepErr error
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		// Fall through: the shared file may already be gone and its owed HUP
		// must still be replayed below.
		sweepErr = fmt.Errorf("read resolver directory: %w", readErr)
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		content, err := os.ReadFile(path)
		if err != nil {
			// An unreadable file cannot be classified, so it may be a managed
			// file left behind — that must fail the sweep, not pass silently.
			// A file that vanished since ReadDir is simply gone.
			if !errors.Is(err, os.ErrNotExist) {
				sweepErr = errors.Join(sweepErr, fmt.Errorf("read resolver file %s: %w", entry.Name(), err))
			}
			continue
		}
		if !resolverset.Managed(content) {
			continue
		}
		pendingHUP = true
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			sweepErr = errors.Join(sweepErr, fmt.Errorf("remove resolver file %s: %w", entry.Name(), err))
			continue
		}
		removed = true
	}
	if removed || pendingHUP {
		sweepErr = errors.Join(sweepErr, reloadResolverCache())
	}
	return sweepErr
}

// reloadResolverCache HUPs mDNSResponder (its pickup of resolver-file changes
// is undocumented). pendingHUP is cleared only on success, so failed reloads
// are retried by a later call even when the file set is already converged.
// Callers hold resolverSyncMu and set pendingHUP before mutating files.
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
	nonRegular := map[string]bool{}
	entries, err := os.ReadDir(directory)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read resolver directory: %w", err)
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			nonRegular[entry.Name()] = true
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
	var syncErr error
	for _, name := range create {
		if nonRegular[name] {
			// A symlink/FIFO/etc squatting on the name: writing would follow
			// it as root. Not ours; report and keep converging the rest.
			syncErr = errors.Join(syncErr, fmt.Errorf("resolver path %s exists but is not a regular file; remove it manually", filepath.Join(directory, name)))
			continue
		}
		if err := os.MkdirAll(directory, 0o755); err != nil {
			syncErr = errors.Join(syncErr, fmt.Errorf("create resolver directory: %w", err))
			continue
		}
		if err := os.WriteFile(filepath.Join(directory, name), []byte(resolverset.Content(port)), 0o644); err != nil {
			syncErr = errors.Join(syncErr, fmt.Errorf("write resolver file %s: %w", name, err))
		}
	}
	for _, name := range remove {
		if err := os.Remove(filepath.Join(directory, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			syncErr = errors.Join(syncErr, fmt.Errorf("remove resolver file %s: %w", name, err))
		}
	}
	if pendingHUP {
		syncErr = errors.Join(syncErr, reloadResolverCache())
	}
	return syncErr
}
