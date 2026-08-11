package cluster

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
)

const (
	DefaultMemoryMiB = 2048
	DefaultCPUs      = 2
	DefaultDiskGiB   = 20
	MaxSubnetIndex   = 255
	firstNodeHost    = 2
	lastNodeHost     = 179
	// LegacyImageArchitecture is the architecture of every cluster image
	// created before architecture was persisted in cluster state.
	LegacyImageArchitecture = "arm64"
)

type Role string

const (
	RoleControlPlane Role = "control-plane"
	RoleWorker       Role = "worker"
)

type NodeDefaults struct {
	MemoryMiB int `json:"memoryMiB"`
	CPUs      int `json:"cpus"`
	DiskGiB   int `json:"diskGiB"`
}

type Node struct {
	Name string `json:"name"`
	Role Role   `json:"role"`
	MAC  string `json:"mac"`
	IP   string `json:"ip"`
}

type Cluster struct {
	Name          string       `json:"name"`
	Index         int          `json:"index"`
	SubnetIndex   int          `json:"subnetIndex"`
	ControlPlanes int          `json:"controlPlanes"`
	Workers       int          `json:"workers"`
	NodeDefaults  NodeDefaults `json:"nodeDefaults"`
	// Per-role overrides; nil means "use NodeDefaults".
	ControlPlaneDefaults *NodeDefaults `json:"controlPlaneDefaults,omitempty"`
	WorkerDefaults       *NodeDefaults `json:"workerDefaults,omitempty"`
	BGP                  bool          `json:"bgp,omitempty"`
	Nodes                []Node        `json:"nodes"`
	Schematic            string        `json:"schematic,omitempty"`
	TalosVersion         string        `json:"talosVersion,omitempty"`
	ImageArchitecture    string        `json:"imageArchitecture,omitempty"`
	// Domain is the canonical cluster domain when explicitly chosen at
	// create; empty means the default, <name>.k8s.test. AllowUnsafeDomain
	// records the opt-in the user passed for it, so emitted config
	// round-trips regardless of how the safe-TLD policy evolves.
	Domain            string `json:"domain,omitempty"`
	AllowUnsafeDomain bool   `json:"allowUnsafeDomain,omitempty"`
}

// DefaultDomainSuffix is the suffix under which default cluster domains live.
const DefaultDomainSuffix = "k8s.test"

// EffectiveDomain returns the domain this cluster is reachable under.
func (c Cluster) EffectiveDomain() string {
	if c.Domain != "" {
		return c.Domain
	}
	return c.Name + "." + DefaultDomainSuffix
}

// DomainInUse reports whether domain is already the effective domain of an
// existing cluster. Exact matches only: nested domains are allowed and
// resolve longest-suffix-wins.
func DomainInUse(domain string, clusters []Cluster) bool {
	for _, item := range clusters {
		if item.EffectiveDomain() == domain {
			return true
		}
	}
	return false
}

// DefaultsFor resolves the effective sizing for a node role.
func (c Cluster) DefaultsFor(role Role) NodeDefaults {
	switch {
	case role == RoleControlPlane && c.ControlPlaneDefaults != nil:
		return *c.ControlPlaneDefaults
	case role == RoleWorker && c.WorkerDefaults != nil:
		return *c.WorkerDefaults
	}
	return c.NodeDefaults
}

func New(name string, subnetIndex, controlPlanes, workers int, defaults NodeDefaults) (Cluster, error) {
	if err := validName(name); err != nil {
		return Cluster{}, err
	}
	if subnetIndex < 0 || subnetIndex > MaxSubnetIndex {
		return Cluster{}, fmt.Errorf("subnet index must be between 0 and %d", MaxSubnetIndex)
	}
	if controlPlanes < 0 || workers < 0 {
		return Cluster{}, errors.New("node counts cannot be negative")
	}
	if controlPlanes+workers > lastNodeHost-firstNodeHost+1 {
		return Cluster{}, fmt.Errorf("cluster cannot contain more than %d nodes", lastNodeHost-firstNodeHost+1)
	}
	if defaults.MemoryMiB < 0 || defaults.CPUs < 0 || defaults.DiskGiB < 0 {
		return Cluster{}, errors.New("node defaults cannot be negative")
	}

	defaults = applyDefaults(defaults)
	c := Cluster{
		Name:          name,
		Index:         subnetIndex,
		SubnetIndex:   subnetIndex,
		ControlPlanes: controlPlanes,
		Workers:       workers,
		NodeDefaults:  defaults,
		Nodes:         make([]Node, 0, controlPlanes+workers),
	}
	for i := 1; i <= controlPlanes; i++ {
		c.Nodes = append(c.Nodes, newNode(
			name,
			fmt.Sprintf("%s-cp-%d", name, i),
			RoleControlPlane,
			reservationIP(subnetIndex, firstNodeHost+len(c.Nodes)),
		))
	}
	for i := 1; i <= workers; i++ {
		c.Nodes = append(c.Nodes, newNode(
			name,
			fmt.Sprintf("%s-worker-%d", name, i),
			RoleWorker,
			reservationIP(subnetIndex, firstNodeHost+len(c.Nodes)),
		))
	}

	return c, nil
}

// LowestFreeSubnetIndex returns the first unallocated 172.30.n.0/24 subnet.
func LowestFreeSubnetIndex(clusters []Cluster) (int, error) {
	used := allocatedSubnetIndexes(clusters)
	for index, allocated := range used {
		if !allocated {
			return index, nil
		}
	}
	return 0, errors.New("all cluster subnets are allocated")
}

// Gateway returns the host-side gateway address for a cluster's subnet.
func Gateway(index int) string {
	return fmt.Sprintf("172.30.%d.1", index)
}

// SubnetCIDR returns the cluster's vmnet subnet.
func SubnetCIDR(index int) string {
	return fmt.Sprintf("172.30.%d.0/24", index)
}

func DeterministicMAC(clusterName, nodeName string) string {
	digest := sha256.Sum256([]byte(clusterName + "/" + nodeName))
	mac := net.HardwareAddr{0x52, 0x54, 0x00, digest[0], digest[1], digest[2]}
	return mac.String()
}

func newNode(clusterName, nodeName string, role Role, ip string) Node {
	return Node{
		Name: nodeName,
		Role: role,
		MAC:  DeterministicMAC(clusterName, nodeName),
		IP:   ip,
	}
}

func applyDefaults(defaults NodeDefaults) NodeDefaults {
	if defaults.MemoryMiB == 0 {
		defaults.MemoryMiB = DefaultMemoryMiB
	}
	if defaults.CPUs == 0 {
		defaults.CPUs = DefaultCPUs
	}
	if defaults.DiskGiB == 0 {
		defaults.DiskGiB = DefaultDiskGiB
	}
	return defaults
}
