package main

import (
	"fmt"

	"github.com/randax/talos-box/internal/daemon"
)

// cacheRefStatus answers "is this image ref in the mirror cache" for one
// reference. The aggregate listing counts blobs per upstream, which cannot
// answer that question, and the on-disk entries are opaque hashes (#406).
type cacheRefStatus struct {
	Ref    string `json:"ref"`
	Cached bool   `json:"cached"`
	// Reason is why an uncached ref is not usable offline: the same reason
	// `cache warm --check` reports, since it is the same verification.
	Reason string `json:"reason,omitempty"`
}

// runCacheListRef reports whether one image reference is cached completely
// enough to be served offline. It is a query, not a gate, so an uncached ref is
// an answer rather than an error; `tbx cache warm --check` is the gate.
func (c cli) runCacheListRef(ref, outputFormat string) error {
	if err := daemon.ValidateWarmRef(ref); err != nil {
		return err
	}
	if err := c.ensureCacheWarmSupport(); err != nil {
		return err
	}
	var result daemon.CacheCheckResult
	if err := c.call("cache.check", daemon.CacheCheckArgs{Refs: []string{ref}}, &result); err != nil {
		return err
	}
	entry, err := cacheCheckEntryForRef(ref, result)
	if err != nil {
		return err
	}
	status := cacheRefStatus{Ref: entry.Ref, Cached: entry.Status == daemon.CacheCheckStatusComplete}
	if !status.Cached {
		status.Reason = entry.Reason
	}
	if outputFormat == "json" {
		return encodeJSON(c.out, status)
	}
	if status.Cached {
		_, err = fmt.Fprintf(c.out, "%s: cached\n", status.Ref)
		return err
	}
	_, err = fmt.Fprintf(c.out, "%s: not cached (%s)\n", status.Ref, status.Reason)
	return err
}
