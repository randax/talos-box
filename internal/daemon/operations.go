package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/helper"
	"github.com/randax/talos-box/internal/vm"
)

type createArgs struct {
	Name          string                `json:"name"`
	ControlPlanes *int                  `json:"controlPlanes"`
	Workers       *int                  `json:"workers"`
	Node          cluster.NodeDefaults  `json:"node"`
	NodeDefaults  cluster.NodeDefaults  `json:"nodeDefaults"`
	ControlPlane  *cluster.NodeDefaults `json:"controlPlane,omitempty"`
	Worker        *cluster.NodeDefaults `json:"worker,omitempty"`
	BGP           bool                  `json:"bgp,omitempty"`
	Schematic     string                `json:"schematic"`
	Version       string                `json:"version"`
	TalosVersion  string                `json:"talosVersion"`
}

type nameArgs struct {
	Name string `json:"name"`
}

type destroyArgs struct {
	Name  string `json:"name"`
	Force bool   `json:"force"`
}

type nodeArgs struct {
	Cluster string       `json:"cluster"`
	Name    string       `json:"name"`
	Role    cluster.Role `json:"role"`
}

type statusArgs struct {
	Cluster string `json:"cluster"`
	Name    string `json:"name"`
}

type cachePullArgs struct {
	Schematic    string `json:"schematic"`
	Version      string `json:"version"`
	TalosVersion string `json:"talosVersion"`
}

// ClusterSummary is the compact cluster.list result.
type ClusterSummary struct {
	Name          string               `json:"name"`
	Index         int                  `json:"index"`
	SubnetIndex   int                  `json:"subnetIndex"`
	ControlPlanes int                  `json:"controlPlanes"`
	Workers       int                  `json:"workers"`
	NodeDefaults  cluster.NodeDefaults `json:"nodeDefaults"`
	TalosVersion  string               `json:"talosVersion"`
	Schematic     string               `json:"schematic"`
	BGP           bool                 `json:"bgp"`
	Running       bool                 `json:"running"`
}

// NodeStatus is the observed host-side state of one node.
type NodeStatus struct {
	Name          string       `json:"name"`
	Role          cluster.Role `json:"role"`
	MAC           string       `json:"mac"`
	IP            string       `json:"ip,omitempty"`
	APIDReachable bool         `json:"apidReachable"`
	Phase         Phase        `json:"phase"`
}

// ClusterStatus is the status result for one cluster.
type ClusterStatus struct {
	Name    string       `json:"name"`
	Subnet  string       `json:"subnet"`
	BGP     bool         `json:"bgp"`
	Running bool         `json:"running"`
	Nodes   []NodeStatus `json:"nodes"`
	Hints   []string     `json:"hints,omitempty"`
}

// CachePullResult describes the image made ready by cache.pull.
type CachePullResult struct {
	Schematic string `json:"schematic"`
	Version   string `json:"version"`
	Path      string `json:"path"`
}

func (s *Server) createCluster(raw json.RawMessage) (ClusterSummary, error) {
	var args createArgs
	if err := decodeArgs(raw, &args); err != nil {
		return ClusterSummary{}, err
	}
	controlPlanes, workers := 1, 2
	if args.ControlPlanes != nil {
		controlPlanes = *args.ControlPlanes
	}
	if args.Workers != nil {
		workers = *args.Workers
	}
	if args.Node == (cluster.NodeDefaults{}) {
		args.Node = args.NodeDefaults
	}
	if args.Version == "" {
		args.Version = args.TalosVersion
	}

	dir, err := cluster.Dir(args.Name)
	if err != nil {
		return ClusterSummary{}, err
	}
	if _, err := os.Stat(dir); err == nil {
		return ClusterSummary{}, fmt.Errorf("cluster %q already exists", args.Name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return ClusterSummary{}, fmt.Errorf("inspect cluster directory: %w", err)
	}
	if err := requireHelper(); err != nil {
		return ClusterSummary{}, err
	}

	clusters, err := cluster.List()
	if err != nil {
		return ClusterSummary{}, err
	}
	subnetIndex, err := cluster.LowestFreeSubnetIndex(clusters)
	if err != nil {
		return ClusterSummary{}, err
	}
	item, err := cluster.New(args.Name, subnetIndex, controlPlanes, workers, args.Node)
	if err != nil {
		return ClusterSummary{}, err
	}
	item.ControlPlaneDefaults = args.ControlPlane
	item.WorkerDefaults = args.Worker
	item.BGP = args.BGP
	item.Schematic, item.TalosVersion, err = s.resolveImage(args.Schematic, args.Version)
	if err != nil {
		return ClusterSummary{}, err
	}
	cachedDisk, err := s.cache.Ensure(item.Schematic, item.TalosVersion)
	if err != nil {
		return ClusterSummary{}, err
	}
	if err := cluster.ProvisionDisks(item, cachedDisk); err != nil {
		_ = cluster.Destroy(item.Name)
		return ClusterSummary{}, err
	}
	if err := cluster.Save(item); err != nil {
		_ = cluster.Destroy(item.Name)
		return ClusterSummary{}, err
	}
	if err := s.start(item); err != nil {
		return summary(item, false), fmt.Errorf("cluster created but failed to start: %w", err)
	}
	return summary(item, true), nil
}

func (s *Server) startCluster(raw json.RawMessage) (ClusterSummary, error) {
	var args nameArgs
	if err := decodeArgs(raw, &args); err != nil {
		return ClusterSummary{}, err
	}
	item, err := cluster.Load(args.Name)
	if err != nil {
		return ClusterSummary{}, err
	}
	if err := s.start(item); err != nil {
		return ClusterSummary{}, err
	}
	return summary(item, true), nil
}

func (s *Server) start(item cluster.Cluster) error {
	nodes := s.vms[item.Name]
	if nodes == nil {
		nodes = make(map[string]*vm.VM)
		s.vms[item.Name] = nodes
	}
	var started []string
	for _, node := range item.Nodes {
		if existing := nodes[node.Name]; existing != nil {
			if existing.Active() {
				continue
			}
			if err := existing.Close(); err != nil {
				return fmt.Errorf("release inactive VM %s: %w", node.Name, err)
			}
			delete(nodes, node.Name)
		}
		machine, err := newVM(item, node)
		if err != nil {
			rollbackErr := s.rollbackStarted(item.Name, nodes, started)
			return errors.Join(fmt.Errorf("create VM %s: %w", node.Name, err), rollbackErr)
		}
		nodes[node.Name] = machine
		started = append(started, node.Name)
		if err := machine.Start(); err != nil {
			rollbackErr := s.rollbackStarted(item.Name, nodes, started)
			return errors.Join(fmt.Errorf("start VM %s: %w", node.Name, err), rollbackErr)
		}
	}
	return nil
}

func (s *Server) rollbackStarted(clusterName string, nodes map[string]*vm.VM, names []string) error {
	return s.closeNodes(clusterName, nodes, names)
}

func newVM(item cluster.Cluster, node cluster.Node) (*vm.VM, error) {
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		return nil, err
	}
	networkFile, cleanup, err := attachNetwork(item, node)
	if err != nil {
		return nil, err
	}
	sizing := item.DefaultsFor(node.Role)
	machine, err := vm.New(vm.Config{
		CPUs:              sizing.CPUs,
		MemoryMiB:         sizing.MemoryMiB,
		DiskPath:          filepath.Join(dir, node.Name+".img"),
		MAC:               node.MAC,
		NetworkFile:       networkFile,
		NetworkCleanup:    cleanup,
		EFIVarsPath:       filepath.Join(dir, node.Name+".efi"),
		ConsoleSocketPath: filepath.Join(dir, node.Name+".console.sock"),
	})
	if err != nil {
		_ = networkFile.Close()
		return nil, errors.Join(err, cleanup())
	}
	return machine, nil
}

func attachNetwork(item cluster.Cluster, node cluster.Node) (*os.File, func() error, error) {
	client, err := helper.Connect()
	if err != nil {
		return nil, nil, helperInstallError(err)
	}
	fd, attachErr := client.Attach(item.Name, item.SubnetIndex, node.Name)
	_ = client.Close()
	if attachErr != nil {
		return nil, nil, fmt.Errorf("attach helper network: %w", attachErr)
	}
	file := os.NewFile(uintptr(fd), item.Name+"/"+node.Name+".network")
	// Detach is best-effort teardown: the helper reclaims the vmnet interface
	// when the VM's fd closes, and explicit detach races that cleanup from
	// either side. A cleanup problem must never wedge stop/destroy.
	cleanup := func() error {
		client, err := helper.Connect()
		if err != nil {
			log.Printf("detach network for %s: %v (ignored)", node.Name, err)
			return nil
		}
		defer func() { _ = client.Close() }()
		if err := client.Detach(item.Name, node.Name); err != nil {
			log.Printf("detach network for %s: %v (ignored)", node.Name, err)
		}
		return nil
	}
	return file, cleanup, nil
}

func helperInstallError(err error) error {
	return fmt.Errorf("network helper unavailable; run `sudo tbx system install`: %w", err)
}

func requireHelper() error {
	client, err := helper.Connect()
	if err != nil {
		return helperInstallError(err)
	}
	defer func() { _ = client.Close() }()
	if err := client.Ping(); err != nil {
		return helperInstallError(err)
	}
	return nil
}

func (s *Server) stopCluster(raw json.RawMessage) (ClusterSummary, error) {
	var args nameArgs
	if err := decodeArgs(raw, &args); err != nil {
		return ClusterSummary{}, err
	}
	item, err := cluster.Load(args.Name)
	if err != nil {
		return ClusterSummary{}, err
	}
	if err := s.stop(item.Name); err != nil {
		return ClusterSummary{}, err
	}
	return summary(item, false), nil
}

func (s *Server) stop(name string) error {
	nodes := s.vms[name]
	if len(nodes) == 0 {
		delete(s.vms, name)
		return nil
	}
	return s.closeNodes(name, nodes, sortedNodeNames(nodes))
}

func (s *Server) closeNodes(clusterName string, nodes map[string]*vm.VM, names []string) error {
	type result struct {
		name string
		err  error
	}
	results := make(chan result, len(names))
	for _, name := range names {
		machine := nodes[name]
		go func() { results <- result{name: name, err: machine.Close()} }()
	}
	errorsByName := make(map[string]error, len(names))
	for range names {
		item := <-results
		errorsByName[item.name] = item.err
	}

	var resultErr error
	for _, name := range names {
		if err := errorsByName[name]; err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("stop VM %s: %w", name, err))
			continue
		}
		delete(nodes, name)
	}
	if len(nodes) == 0 {
		delete(s.vms, clusterName)
	}
	return resultErr
}

func (s *Server) destroyCluster(raw json.RawMessage) (map[string]string, error) {
	var args destroyArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if !args.Force {
		return nil, errors.New("cluster.destroy requires force=true")
	}
	dir, err := cluster.Dir(args.Name)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("cluster %q does not exist", args.Name)
	}
	// stop what we can, but a partially-destroyed cluster (state dir present,
	// cluster.json gone) must still be removable
	if _, loadErr := cluster.Load(args.Name); loadErr == nil {
		if err := s.stop(args.Name); err != nil {
			return nil, err
		}
	}
	if err := cluster.Destroy(args.Name); err != nil {
		return nil, err
	}
	return map[string]string{"name": args.Name}, nil
}

func (s *Server) listClusters() ([]ClusterSummary, error) {
	items, err := cluster.List()
	if err != nil {
		return nil, err
	}
	result := make([]ClusterSummary, 0, len(items))
	for _, item := range items {
		result = append(result, summary(item, s.clusterRunning(item.Name)))
	}
	return result, nil
}

func (s *Server) addNode(raw json.RawMessage) (NodeStatus, error) {
	var args nodeArgs
	if err := decodeArgs(raw, &args); err != nil {
		return NodeStatus{}, err
	}
	if args.Role == "" {
		args.Role = cluster.RoleWorker
	}
	item, err := cluster.Load(args.Cluster)
	if err != nil {
		return NodeStatus{}, err
	}
	cachedDisk, err := s.cachedDisk(item)
	if err != nil {
		return NodeStatus{}, err
	}
	node, err := cluster.AddNode(&item, args.Role, args.Name)
	if err != nil {
		return NodeStatus{}, err
	}
	if err := cluster.ProvisionDisks(item, cachedDisk); err != nil {
		_ = removeNodeFiles(item.Name, node.Name)
		return NodeStatus{}, err
	}
	if err := cluster.Save(item); err != nil {
		_ = removeNodeFiles(item.Name, node.Name)
		return NodeStatus{}, err
	}
	if s.clusterRunning(item.Name) {
		machine, err := newVM(item, node)
		if err != nil {
			return nodeStatus(node, item.SubnetIndex, false), fmt.Errorf("node added but failed to create VM: %w", err)
		}
		if err := machine.Start(); err != nil {
			startErr := fmt.Errorf("node added but failed to start: %w", err)
			if closeErr := machine.Close(); closeErr != nil {
				s.vms[item.Name][node.Name] = machine
				return nodeStatus(node, item.SubnetIndex, false), errors.Join(startErr, fmt.Errorf("release failed VM: %w", closeErr))
			}
			return nodeStatus(node, item.SubnetIndex, false), startErr
		}
		s.vms[item.Name][node.Name] = machine
	}
	return nodeStatus(node, item.SubnetIndex, s.nodeRunning(item.Name, node.Name)), nil
}

func (s *Server) removeNode(raw json.RawMessage) (NodeStatus, error) {
	var args nodeArgs
	if err := decodeArgs(raw, &args); err != nil {
		return NodeStatus{}, err
	}
	item, err := cluster.Load(args.Cluster)
	if err != nil {
		return NodeStatus{}, err
	}
	node, err := cluster.RemoveNode(&item, args.Name)
	if err != nil {
		return NodeStatus{}, err
	}
	if machine := s.vms[item.Name][node.Name]; machine != nil {
		if err := machine.Close(); err != nil {
			return NodeStatus{}, fmt.Errorf("stop node %s: %w", node.Name, err)
		}
		delete(s.vms[item.Name], node.Name)
	}
	if err := cluster.Save(item); err != nil {
		return NodeStatus{}, err
	}
	if err := removeNodeFiles(item.Name, node.Name); err != nil {
		return NodeStatus{}, err
	}
	return nodeStatus(node, item.SubnetIndex, false), nil
}

func (s *Server) status(raw json.RawMessage) ([]ClusterStatus, error) {
	var args statusArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.Cluster == "" {
		args.Cluster = args.Name
	}
	var items []cluster.Cluster
	if args.Cluster != "" {
		item, err := cluster.Load(args.Cluster)
		if err != nil {
			return nil, err
		}
		items = []cluster.Cluster{item}
	} else {
		var err error
		items, err = cluster.List()
		if err != nil {
			return nil, err
		}
	}

	result := make([]ClusterStatus, 0, len(items))
	for _, item := range items {
		clusterStatus := ClusterStatus{Name: item.Name, Subnet: cluster.SubnetCIDR(item.SubnetIndex), BGP: item.BGP, Running: s.clusterRunning(item.Name)}
		for _, node := range item.Nodes {
			clusterStatus.Nodes = append(clusterStatus.Nodes, nodeStatus(node, item.SubnetIndex, s.nodeRunning(item.Name, node.Name)))
		}
		clusterStatus.Hints = Hints(clusterStatus)
		result = append(result, clusterStatus)
	}
	return result, nil
}

func (s *Server) nodeRunning(clusterName, nodeName string) bool {
	machine := s.vms[clusterName][nodeName]
	return machine != nil && machine.Active()
}

func (s *Server) clusterRunning(name string) bool {
	for _, machine := range s.vms[name] {
		if machine.Active() {
			return true
		}
	}
	return false
}

func nodeStatus(node cluster.Node, subnetIndex int, vmRunning bool) NodeStatus {
	ip := vm.LeaseIP(node.MAC, subnetIndex)
	probe := ProbeResult{}
	if ip != "" {
		probe = probeAPID(ip)
	}
	return NodeStatus{
		Name:          node.Name,
		Role:          node.Role,
		MAC:           node.MAC,
		IP:            ip,
		APIDReachable: probe.Dialed,
		Phase:         ClassifyPhase(vmRunning, probe),
	}
}

func (s *Server) pullCache(raw json.RawMessage) (CachePullResult, error) {
	var args cachePullArgs
	if err := decodeArgs(raw, &args); err != nil {
		return CachePullResult{}, err
	}
	if args.Version == "" {
		args.Version = args.TalosVersion
	}
	schematic, talosVersion, err := s.resolveImage(args.Schematic, args.Version)
	if err != nil {
		return CachePullResult{}, err
	}
	path, err := s.cache.Ensure(schematic, talosVersion)
	if err != nil {
		return CachePullResult{}, err
	}
	return CachePullResult{Schematic: schematic, Version: talosVersion, Path: path}, nil
}

func (s *Server) pruneCache() (map[string]int, error) {
	entries, err := s.cache.List()
	if err != nil {
		return nil, err
	}
	if err := s.cache.Prune(); err != nil {
		return nil, err
	}
	return map[string]int{"removed": len(entries)}, nil
}

func (s *Server) resolveImage(schematic, talosVersion string) (string, string, error) {
	if talosVersion == "" {
		talosVersion = DefaultTalosVersion
	}
	if schematic == "" {
		if s.defaultSchematic == "" {
			var err error
			s.defaultSchematic, err = s.cache.Schematic()
			if err != nil {
				return "", "", err
			}
		}
		schematic = s.defaultSchematic
	}
	return schematic, talosVersion, nil
}

func (s *Server) cachedDisk(item cluster.Cluster) (string, error) {
	schematic, talosVersion, err := s.resolveImage(item.Schematic, item.TalosVersion)
	if err != nil {
		return "", err
	}
	return s.cache.Ensure(schematic, talosVersion)
}

func summary(item cluster.Cluster, running bool) ClusterSummary {
	return ClusterSummary{
		Name:          item.Name,
		Index:         item.Index,
		SubnetIndex:   item.SubnetIndex,
		ControlPlanes: item.ControlPlanes,
		Workers:       item.Workers,
		NodeDefaults:  item.NodeDefaults,
		TalosVersion:  item.TalosVersion,
		Schematic:     item.Schematic,
		BGP:           item.BGP,
		Running:       running,
	}
}
