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
}

type Cluster struct {
	Name          string       `json:"name"`
	Index         int          `json:"index"`
	SubnetIndex   int          `json:"subnetIndex"`
	ControlPlanes int          `json:"controlPlanes"`
	Workers       int          `json:"workers"`
	NodeDefaults  NodeDefaults `json:"nodeDefaults"`
	Nodes         []Node       `json:"nodes"`
	Schematic     string       `json:"schematic,omitempty"`
	TalosVersion  string       `json:"talosVersion,omitempty"`
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
		c.Nodes = append(c.Nodes, newNode(name, fmt.Sprintf("%s-cp-%d", name, i), RoleControlPlane))
	}
	for i := 1; i <= workers; i++ {
		c.Nodes = append(c.Nodes, newNode(name, fmt.Sprintf("%s-worker-%d", name, i), RoleWorker))
	}

	return c, nil
}

// LowestFreeSubnetIndex returns the first unallocated 172.30.n.0/24 subnet.
func LowestFreeSubnetIndex(clusters []Cluster) (int, error) {
	used := make([]bool, MaxSubnetIndex+1)
	for _, item := range clusters {
		if item.SubnetIndex >= 0 && item.SubnetIndex <= MaxSubnetIndex {
			used[item.SubnetIndex] = true
		}
	}
	for index, allocated := range used {
		if !allocated {
			return index, nil
		}
	}
	return 0, errors.New("all cluster subnets are allocated")
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

func newNode(clusterName, nodeName string, role Role) Node {
	return Node{
		Name: nodeName,
		Role: role,
		MAC:  DeterministicMAC(clusterName, nodeName),
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
