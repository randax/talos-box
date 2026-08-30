package config

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/randax/talos-box/internal/cluster"
)

// Marshal renders a Config as canonical talosbox.yaml — the exact document
// Parse accepts. Used by `tbx cluster create` to print the file it implied.
func Marshal(cfg Config) string {
	var b strings.Builder
	b.WriteString("version: 1\n")
	if !cfg.Talos.IsZero() {
		b.WriteString("talos:\n")
		writeTalosFields(&b, cfg.Talos, "  ")
	}
	b.WriteString("clusters:\n")
	for _, c := range cfg.Clusters {
		fmt.Fprintf(&b, "  - name: %s\n", c.Name)
		if c.Hypervisor != "" {
			fmt.Fprintf(&b, "    hypervisor: %s\n", c.Hypervisor)
		}
		fmt.Fprintf(&b, "    controlPlanes: %d\n", c.ControlPlanes)
		fmt.Fprintf(&b, "    workers: %d\n", c.Workers)
		if c.CNI != "" {
			fmt.Fprintf(&b, "    cni: %s\n", c.CNI)
			if c.CSI != "" {
				fmt.Fprintf(&b, "    csi: %s\n", c.CSI)
			}
			fmt.Fprintf(&b, "    lb: %t\n", c.LB)
			if c.BGP {
				b.WriteString("    bgp: true\n")
			}
			if c.Hubble {
				b.WriteString("    hubble: true\n")
			}
			if c.DisableKubeletMemoryProtection {
				b.WriteString("    kubeletMemoryProtection: false\n")
			}
		}
		if c.Domain != "" {
			fmt.Fprintf(&b, "    domain: %s\n", c.Domain)
		}
		if c.AllowUnsafeDomain {
			b.WriteString("    allowUnsafeDomain: true\n")
		}
		// A resolved talos equal to the file-level block is pure inheritance,
		// and a zero one has nothing to say; only a divergence needs its own
		// stanza.
		if !c.Talos.IsZero() && !c.Talos.Equal(cfg.Talos) {
			b.WriteString("    talos:\n")
			writeTalosFields(&b, c.Talos, "      ")
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

func writeTalosFields(b *strings.Builder, t TalosSpec, indent string) {
	if t.Version != "" {
		fmt.Fprintf(b, "%sversion: %s\n", indent, t.Version)
	}
	if t.Schematic != "" {
		fmt.Fprintf(b, "%sschematic: %s\n", indent, t.Schematic)
	}
	if t.Extensions != nil {
		quoted := make([]string, len(t.Extensions))
		for i, extension := range t.Extensions {
			// Double-quote each element so names carrying YAML flow syntax
			// (commas, brackets) round-trip through Parse unchanged.
			quoted[i] = strconv.Quote(extension)
		}
		fmt.Fprintf(b, "%sextensions: [%s]\n", indent, strings.Join(quoted, ", "))
	}
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
