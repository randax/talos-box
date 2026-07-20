package cluster

import (
	"fmt"
	"strconv"
	"strings"
)

// AddNode adds a uniquely named node, generating the next role name when name is empty.
func AddNode(c *Cluster, role Role, name string) (Node, error) {
	if role != RoleControlPlane && role != RoleWorker {
		return Node{}, fmt.Errorf("invalid node role %q", role)
	}
	if name == "" {
		name = nextNodeName(*c, role)
	}
	if err := validName(name); err != nil {
		return Node{}, fmt.Errorf("invalid node name %q: %w", name, err)
	}
	for _, node := range c.Nodes {
		if node.Name == name {
			return Node{}, fmt.Errorf("node %q already exists", name)
		}
	}

	node := newNode(c.Name, name, role)
	c.Nodes = append(c.Nodes, node)
	if role == RoleControlPlane {
		c.ControlPlanes++
	} else {
		c.Workers++
	}
	return node, nil
}

// RemoveNode removes name from the cluster model and returns the removed node.
func RemoveNode(c *Cluster, name string) (Node, error) {
	for i, node := range c.Nodes {
		if node.Name != name {
			continue
		}
		c.Nodes = append(c.Nodes[:i], c.Nodes[i+1:]...)
		if node.Role == RoleControlPlane {
			c.ControlPlanes--
		} else {
			c.Workers--
		}
		return node, nil
	}
	return Node{}, fmt.Errorf("node %q not found", name)
}

func nextNodeName(c Cluster, role Role) string {
	label := "worker"
	if role == RoleControlPlane {
		label = "cp"
	}
	prefix := c.Name + "-" + label + "-"
	maxIndex := 0
	for _, node := range c.Nodes {
		if !strings.HasPrefix(node.Name, prefix) {
			continue
		}
		index, err := strconv.Atoi(strings.TrimPrefix(node.Name, prefix))
		if err == nil && index > maxIndex {
			maxIndex = index
		}
	}
	return prefix + strconv.Itoa(maxIndex+1)
}
