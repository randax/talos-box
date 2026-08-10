//go:build darwin

package helper

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/domain"
	"github.com/randax/talos-box/internal/resolverset"
)

func installHostResolver(port int) error { return installResolver(resolverPath, port) }

// syncDomainResolvers converges /etc/resolver/<domain> files for clusters
// with custom domains. The helper's privilege boundary is a state-derived
// allow-list (SPEC §11): every requested name must be a valid canonical
// domain, and only files carrying the ownership marker are ever removed. The
// shared default-suffix file is not managed here.
func syncDomainResolvers(domains []string, port int) error {
	wanted := make([]string, 0, len(domains))
	for _, name := range domains {
		canonical, err := domain.Validate(name, true)
		if err != nil {
			return fmt.Errorf("refuse resolver sync: %w", err)
		}
		if canonical != name {
			return fmt.Errorf("refuse resolver sync: domain %q is not canonical", name)
		}
		if canonical == cluster.DefaultDomainSuffix {
			continue // covered by the shared resolver file
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
	if len(create) != 0 || len(remove) != 0 {
		// mDNSResponder's pickup of resolver-file changes is undocumented; a
		// HUP is the established way to make new domains work immediately.
		if err := exec.Command("/usr/bin/killall", "-HUP", "mDNSResponder").Run(); err != nil {
			return fmt.Errorf("signal mDNSResponder: %w", err)
		}
	}
	return nil
}

func uninstallHostResolver() error {
	err := os.Remove(resolverPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove resolver file: %w", err)
	}
	return nil
}
