// Package config parses talosbox.yaml, the declarative cluster description
// that `tbx up` reconciles against (SPEC §9).
package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/randax/talos-box/internal/cluster"
)

// Defaults per SPEC §8.
var defaultNode = cluster.NodeDefaults{MemoryMiB: cluster.DefaultMemoryMiB, CPUs: cluster.DefaultCPUs, DiskGiB: cluster.DefaultDiskGiB}

const (
	defaultControlPlanes = 1
	defaultWorkers       = 2
)

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// Config is a fully parsed, defaulted, validated talosbox.yaml.
type Config struct {
	Talos    TalosSpec
	Clusters []ClusterSpec
}

// TalosSpec pins the image; empty fields mean "the daemon's pinned default".
type TalosSpec struct {
	Version   string
	Schematic string
}

// ClusterSpec is one desired cluster with all defaults applied.
type ClusterSpec struct {
	Name          string
	ControlPlanes int
	Workers       int
	BGP           bool
	Node          cluster.NodeDefaults
	// ControlPlane/Worker are set only when the file overrides the role.
	ControlPlane *cluster.NodeDefaults
	Worker       *cluster.NodeDefaults
}

type rawConfig struct {
	Version  int          `yaml:"version"`
	Talos    rawTalos     `yaml:"talos"`
	Clusters []rawCluster `yaml:"clusters"`
}

type rawTalos struct {
	Version   string `yaml:"version"`
	Schematic string `yaml:"schematic"`
}

type rawCluster struct {
	Name          string   `yaml:"name"`
	ControlPlanes *int     `yaml:"controlPlanes"`
	Workers       *int     `yaml:"workers"`
	BGP           bool     `yaml:"bgp"`
	Node          rawNode  `yaml:"node"`
	ControlPlane  *rawNode `yaml:"controlPlane"`
	Worker        *rawNode `yaml:"worker"`
}

type rawNode struct {
	Memory   string `yaml:"memory"`
	CPUs     int    `yaml:"cpus"`
	DiskSize string `yaml:"diskSize"`
}

// Parse decodes, defaults, and validates a talosbox.yaml document.
func Parse(data []byte) (Config, error) {
	var raw rawConfig
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return Config{}, fmt.Errorf("parse talosbox.yaml: %w", err)
	}
	if raw.Version != 1 {
		return Config{}, fmt.Errorf("unsupported version %d (this tbx understands version 1)", raw.Version)
	}
	if len(raw.Clusters) == 0 {
		return Config{}, fmt.Errorf("talosbox.yaml must describe at least one cluster")
	}

	cfg := Config{Talos: TalosSpec(raw.Talos)}
	seen := map[string]bool{}
	for _, rc := range raw.Clusters {
		if !nameRe.MatchString(rc.Name) {
			return Config{}, fmt.Errorf("cluster name %q is invalid (lowercase letters, digits, hyphens)", rc.Name)
		}
		if seen[rc.Name] {
			return Config{}, fmt.Errorf("duplicate cluster name %q", rc.Name)
		}
		seen[rc.Name] = true

		spec := ClusterSpec{Name: rc.Name, ControlPlanes: defaultControlPlanes, Workers: defaultWorkers, BGP: rc.BGP}
		if rc.ControlPlanes != nil {
			spec.ControlPlanes = *rc.ControlPlanes
		}
		if rc.Workers != nil {
			spec.Workers = *rc.Workers
		}
		if spec.ControlPlanes < 1 {
			return Config{}, fmt.Errorf("cluster %q needs at least one control plane", rc.Name)
		}
		if spec.Workers < 0 {
			return Config{}, fmt.Errorf("cluster %q has negative workers", rc.Name)
		}

		node, err := resolveNode(defaultNode, rc.Node)
		if err != nil {
			return Config{}, fmt.Errorf("cluster %q node: %w", rc.Name, err)
		}
		spec.Node = node
		if rc.ControlPlane != nil {
			cp, err := resolveNode(node, *rc.ControlPlane)
			if err != nil {
				return Config{}, fmt.Errorf("cluster %q controlPlane: %w", rc.Name, err)
			}
			spec.ControlPlane = &cp
		}
		if rc.Worker != nil {
			w, err := resolveNode(node, *rc.Worker)
			if err != nil {
				return Config{}, fmt.Errorf("cluster %q worker: %w", rc.Name, err)
			}
			spec.Worker = &w
		}
		cfg.Clusters = append(cfg.Clusters, spec)
	}
	return cfg, nil
}

// resolveNode overlays the fields set in raw onto base.
func resolveNode(base cluster.NodeDefaults, raw rawNode) (cluster.NodeDefaults, error) {
	out := base
	if raw.Memory != "" {
		mib, err := parseSizeMiB(raw.Memory)
		if err != nil {
			return out, err
		}
		out.MemoryMiB = mib
	}
	if raw.CPUs != 0 {
		out.CPUs = raw.CPUs
	}
	if raw.DiskSize != "" {
		mib, err := parseSizeMiB(raw.DiskSize)
		if err != nil {
			return out, err
		}
		if mib%1024 != 0 {
			return out, fmt.Errorf("size %q: disk sizes must be whole GiB", raw.DiskSize)
		}
		out.DiskGiB = mib / 1024
	}
	return out, nil
}

// parseSizeMiB accepts "<n>GiB" or "<n>MiB".
func parseSizeMiB(s string) (int, error) {
	var mult int
	var num string
	switch {
	case strings.HasSuffix(s, "GiB"):
		mult, num = 1024, strings.TrimSuffix(s, "GiB")
	case strings.HasSuffix(s, "MiB"):
		mult, num = 1, strings.TrimSuffix(s, "MiB")
	default:
		return 0, fmt.Errorf("size %q: use GiB or MiB (e.g. 2GiB, 1536MiB)", s)
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("size %q: use GiB or MiB (e.g. 2GiB, 1536MiB)", s)
	}
	return n * mult, nil
}
