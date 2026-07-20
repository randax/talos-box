package config

import (
	"fmt"
	"strings"

	"github.com/randax/talos-box/internal/cluster"
)

// Marshal renders a Config as canonical talosbox.yaml — the exact document
// Parse accepts. Used by `tbx cluster create` to print the file it implied.
func Marshal(cfg Config) string {
	var b strings.Builder
	b.WriteString("version: 1\n")
	if cfg.Talos.Version != "" || cfg.Talos.Schematic != "" {
		b.WriteString("talos:\n")
		if cfg.Talos.Version != "" {
			fmt.Fprintf(&b, "  version: %s\n", cfg.Talos.Version)
		}
		if cfg.Talos.Schematic != "" {
			fmt.Fprintf(&b, "  schematic: %s\n", cfg.Talos.Schematic)
		}
	}
	b.WriteString("clusters:\n")
	for _, c := range cfg.Clusters {
		fmt.Fprintf(&b, "  - name: %s\n", c.Name)
		fmt.Fprintf(&b, "    controlPlanes: %d\n", c.ControlPlanes)
		fmt.Fprintf(&b, "    workers: %d\n", c.Workers)
		if c.BGP {
			b.WriteString("    bgp: true\n")
		}
		writeNode(&b, "node", c.Node)
		if c.ControlPlane != nil {
			writeNode(&b, "controlPlane", *c.ControlPlane)
		}
		if c.Worker != nil {
			writeNode(&b, "worker", *c.Worker)
		}
	}
	return b.String()
}

func writeNode(b *strings.Builder, key string, n cluster.NodeDefaults) {
	fmt.Fprintf(b, "    %s:\n", key)
	fmt.Fprintf(b, "      memory: %s\n", sizeString(n.MemoryMiB))
	fmt.Fprintf(b, "      cpus: %d\n", n.CPUs)
	fmt.Fprintf(b, "      diskSize: %dGiB\n", n.DiskGiB)
}

func sizeString(mib int) string {
	if mib%1024 == 0 {
		return fmt.Sprintf("%dGiB", mib/1024)
	}
	return fmt.Sprintf("%dMiB", mib)
}
