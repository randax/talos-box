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
	ControlPlanes int          `json:"controlPlanes"`
	Workers       int          `json:"workers"`
	NodeDefaults  NodeDefaults `json:"nodeDefaults"`
	Nodes         []Node       `json:"nodes"`
}

func New(name string, index, controlPlanes, workers int, defaults NodeDefaults) (Cluster, error) {
	if err := validName(name); err != nil {
		return Cluster{}, err
	}
	if index < 0 || controlPlanes < 0 || workers < 0 {
		return Cluster{}, errors.New("cluster index and node counts cannot be negative")
	}
	if defaults.MemoryMiB < 0 || defaults.CPUs < 0 || defaults.DiskGiB < 0 {
		return Cluster{}, errors.New("node defaults cannot be negative")
	}

	defaults = applyDefaults(defaults)
	c := Cluster{
		Name:          name,
		Index:         index,
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
