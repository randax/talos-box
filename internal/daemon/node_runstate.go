package daemon

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
)

// startNodeLocked launches one node's VM without touching cluster membership.
// A node started into a cluster with nothing else running gets the same
// cluster-start side effects the whole-cluster path performs, so the registry
// mirrors are bound before the first node needs them (#322).
func (s *Server) startNodeLocked(raw json.RawMessage) (NodeStatus, []provisionTask, error) {
	var args nodeArgs
	if err := decodeArgs(raw, &args); err != nil {
		return NodeStatus{}, nil, err
	}
	item, err := cluster.Load(args.Cluster)
	if err != nil {
		return NodeStatus{}, nil, err
	}
	node, err := clusterNode(item, args.Name)
	if err != nil {
		return NodeStatus{}, nil, err
	}
	if s.nodeRunning(item.Name, node.Name) {
		log.Printf("node.start %s/%s: already running", item.Name, node.Name)
		return nodeStatus(node, item.SubnetIndex, true), nil, nil
	}
	// Powering a node on commits the same host memory `cluster start` and
	// `node add` are gated on, so it answers to the same guards: a hard refusal
	// without --force, an advisory finding with it.
	overcommitWarning, err := s.checkOvercommit(item.DefaultsFor(node.Role).MemoryMiB, args.Force)
	if err != nil {
		return NodeStatus{}, nil, err
	}
	firstNode := !s.clusterRunning(item.Name)
	var subnetWarning string
	if firstNode {
		// The subnet was decided at create time and belongs to this cluster, so
		// it is only inspected for advisory routing findings (#271).
		subnetWarning, err = cluster.AttachedSubnetWarning(item.SubnetIndex, s.hostSubnetSources())
		if err != nil {
			return NodeStatus{}, nil, err
		}
	}
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		return NodeStatus{}, nil, err
	}
	hostPressureWarnings, err := s.checkHostPressure(dir, args.Force)
	if err != nil {
		return NodeStatus{}, nil, err
	}
	nodes := s.vms[item.Name]
	if nodes == nil {
		nodes = make(map[string]hypervisor.Machine)
		s.vms[item.Name] = nodes
	}
	if existing := nodes[node.Name]; existing != nil {
		// an inactive machine still holds its host resources until it is released
		if err := existing.Close(); err != nil {
			return NodeStatus{}, nil, fmt.Errorf("release inactive VM %s: %w", node.Name, err)
		}
		delete(nodes, node.Name)
	}
	machine, err := s.launchMachine(item, node, nil)
	if err != nil {
		return NodeStatus{}, nil, fmt.Errorf("create VM %s: %w", node.Name, err)
	}
	nodes[node.Name] = machine
	// node.start is a cold boot: suspended memory for this node is superseded
	// by the fresh launch and must not be left to poison Suspended status or
	// the resume hint. It is only superseded once the launch succeeds —
	// launchMachine never reads the save — so a failed start leaves the memory
	// intact and `cluster resume` still works.
	var discardWarning string
	dropped, discardFailure := discardSavedState(dir, node.Name)
	if dropped {
		discardWarning = discardedSaveStateWarning(node.Name)
	}
	if firstNode {
		go s.bindMirrors(item.SubnetIndex) // async: don't hold opMu across the retry
		if subnetWarning != "" {
			log.Printf("start %s: %s", item.Name, subnetWarning)
		}
	}
	log.Printf("node.start %s/%s: VM started", item.Name, node.Name)
	status := nodeStatus(node, item.SubnetIndex, true)
	status.setWarnings(append([]string{overcommitWarning}, append(hostPressureWarnings, subnetWarning, discardWarning, discardFailure)...)...)
	// beginNodeMutationProvisionLocked schedules nothing while members are
	// still powered off, so only the last stopped one coming back reconciles.
	return status, s.beginNodeMutationProvisionLocked(item), nil
}

// stopNodeLocked powers one node's VM off, leaving it a cluster member with its
// disk intact. Stopping the last running node leaves the cluster stopped, so it
// performs the same teardown the whole-cluster stop does (#322).
func (s *Server) stopNodeLocked(raw json.RawMessage) (NodeStatus, []provisionTask, error) {
	var args nodeArgs
	if err := decodeArgs(raw, &args); err != nil {
		return NodeStatus{}, nil, err
	}
	item, err := cluster.Load(args.Cluster)
	if err != nil {
		return NodeStatus{}, nil, err
	}
	node, err := clusterNode(item, args.Name)
	if err != nil {
		return NodeStatus{}, nil, err
	}
	if !s.nodeRunning(item.Name, node.Name) {
		log.Printf("node.stop %s/%s: already stopped", item.Name, node.Name)
		return nodeStatus(node, item.SubnetIndex, false), nil, nil
	}
	quorumWarning := controlPlaneQuorumWarning(item, node, func(name string) bool {
		return s.nodeRunning(item.Name, name)
	})
	log.Printf("node.stop %s/%s: begin", item.Name, node.Name)
	if err := s.closeNodes(item.Name, s.vms[item.Name], []string{node.Name}); err != nil {
		log.Printf("node.stop %s/%s: stop VM failed: %v", item.Name, node.Name, err)
		return NodeStatus{}, nil, err
	}
	// A recorded `live` phase short-circuits refreshStoragePhases, so leaving it
	// standing would keep reporting storage live over a cluster that just lost a
	// node. Invalidating only drops the memo — the phase is re-probed, not
	// parked at `provisioning`, which only beginProvisionTasksLocked does.
	s.invalidateStoragePhaseLocked(item.Name)
	if !s.clusterRunning(item.Name) {
		s.cancelProvisionLocked(item.Name)
		s.unbindMirrors(item.SubnetIndex)
		log.Printf("node.stop %s/%s: last running node, cluster is stopped", item.Name, node.Name)
	}
	log.Printf("node.stop %s/%s: VM stopped", item.Name, node.Name)
	status := nodeStatus(node, item.SubnetIndex, false)
	status.setWarnings(quorumWarning)
	// No reconcile: stopping a node leaves the cluster short of a member the
	// reconcile's own request still lists, so provisioning could never converge
	// and would only burn the provision timeout before parking storage.
	return status, nil, nil
}

// controlPlaneQuorumWarning is advisory only — `node stop` never blocks — but
// an operator who powers off a control-plane node deserves to know what it
// costs etcd before the cluster stops answering.
func controlPlaneQuorumWarning(item cluster.Cluster, node cluster.Node, running func(string) bool) string {
	if node.Role != cluster.RoleControlPlane {
		return ""
	}
	var total, remaining int
	for _, member := range item.Nodes {
		if member.Role != cluster.RoleControlPlane {
			continue
		}
		total++
		if member.Name != node.Name && running(member.Name) {
			remaining++
		}
	}
	return fmt.Sprintf(
		"stopping control-plane node %s leaves %d of %d control-plane nodes running; etcd quorum requires a majority",
		node.Name, remaining, total,
	)
}

// clusterNode resolves a node verb's node argument against cluster membership,
// so an unknown node is refused before anything is launched or closed.
func clusterNode(item cluster.Cluster, name string) (cluster.Node, error) {
	if name == "" {
		return cluster.Node{}, fmt.Errorf("cluster %q: a node name is required", item.Name)
	}
	for _, node := range item.Nodes {
		if node.Name == name {
			return node, nil
		}
	}
	return cluster.Node{}, fmt.Errorf("cluster %q has no node %q", item.Name, name)
}
