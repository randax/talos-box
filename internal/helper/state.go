package helper

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/randax/talos-box/internal/cluster"
)

// reservationsFileName holds the reservations tbxd last pushed. The helper is
// the only writer, and it never reads a user's home: under the packaged unit it
// runs as an unprivileged system user that cannot open one.
const reservationsFileName = "reservations.json"

// SyncedCluster is one cluster's networking as pushed over net.sync. It carries
// only what host networking needs — the subnet and the DHCP reservations — not
// the caller's full cluster record.
type SyncedCluster struct {
	Name        string       `json:"name"`
	SubnetIndex int          `json:"subnetIndex"`
	Nodes       []SyncedNode `json:"nodes"`
}

// SyncedNode is one node's DHCP reservation. The name is carried so validation
// failures name the node the operator knows.
type SyncedNode struct {
	Name string `json:"name"`
	MAC  string `json:"mac"`
	IP   string `json:"ip"`
}

type syncedState struct {
	Clusters []SyncedCluster `json:"clusters"`
}

// State is the helper's copy of the reservations tbxd owns. A state with
// no directory keeps them in memory only, which loses host networking across a
// helper restart but never fails an otherwise working helper.
type State struct {
	mu       sync.RWMutex
	clusters []cluster.Cluster
	path     string
}

// NewState creates the helper's reservation state. An empty dir keeps the
// reservations in memory only.
func NewState(dir string) *State {
	state := &State{}
	if dir != "" {
		state.path = filepath.Join(dir, reservationsFileName)
	}
	return state
}

// StateDir picks the reservation directory from the process environment.
func StateDir() string { return helperStateDir(os.Getenv) }

// helperStateDir picks the directory the reservations live in: the systemd
// StateDirectory when the unit provides one, an explicit override otherwise,
// and empty when neither is set (memory only).
func helperStateDir(getenv func(string) string) string {
	if directories := getenv("STATE_DIRECTORY"); directories != "" {
		// systemd joins multiple StateDirectory= entries with ':'.
		first, _, _ := strings.Cut(directories, ":")
		return first
	}
	return getenv("TBX_HELPER_STATE_DIR")
}

// Load reads the persisted reservations. A missing file is an empty set: a
// helper that has never been synced still serves. So is an unreadable,
// undecodable, or invalid one, logged rather than fatal: tbxd re-pushes the
// full set on its next start, whereas a fatal load takes the socket unit down
// with the start limit — the failure #467 was about.
func (s *State) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		return nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.clusters = nil
			return nil
		}
		s.clusters = nil
		log.Printf("read helper reservations %s: %v; starting with no reservations until tbxd syncs", s.path, err)
		return nil
	}
	var stored syncedState
	if err := json.Unmarshal(raw, &stored); err != nil {
		s.clusters = nil
		log.Printf("decode helper reservations %s: %v; starting with no reservations until tbxd syncs", s.path, err)
		return nil
	}
	clusters, err := syncedClusters(stored.Clusters)
	if err != nil {
		s.clusters = nil
		log.Printf("validate helper reservations %s: %v; starting with no reservations until tbxd syncs", s.path, err)
		return nil
	}
	s.clusters = clusters
	return nil
}

// Replace validates and adopts a pushed reservation set, persisting it when the
// helper has a state directory. A rejected set leaves the previous one in place
// on disk and in memory.
func (s *State) Replace(in []SyncedCluster) error {
	clusters, err := syncedClusters(in)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.persist(in); err != nil {
		return err
	}
	s.clusters = clusters
	return nil
}

func (s *State) persist(in []SyncedCluster) error {
	if s.path == "" {
		return nil
	}
	raw, err := json.Marshal(syncedState{Clusters: in})
	if err != nil {
		return fmt.Errorf("encode helper reservations: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create helper state directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(s.path), reservationsFileName+".*")
	if err != nil {
		return fmt.Errorf("create helper reservations temp file: %w", err)
	}
	name := temporary.Name()
	defer func() { _ = os.Remove(name) }()
	if err := writeAndClose(temporary, raw); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return fmt.Errorf("set helper reservations permissions: %w", err)
	}
	if err := os.Rename(name, s.path); err != nil {
		return fmt.Errorf("replace helper reservations %s: %w", s.path, err)
	}
	return nil
}

func writeAndClose(file *os.File, raw []byte) error {
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		return fmt.Errorf("write helper reservations: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("write helper reservations: %w", err)
	}
	return nil
}

// Clusters returns the synced reservations.
func (s *State) Clusters() []cluster.Cluster {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.clusters)
}

// SubnetIndexes returns the subnets the synced clusters occupy, sorted and
// deduplicated.
func (s *State) SubnetIndexes() []int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	indexes := make([]int, 0, len(s.clusters))
	for _, item := range s.clusters {
		indexes = append(indexes, item.SubnetIndex)
	}
	slices.Sort(indexes)
	return slices.Compact(indexes)
}

// syncedClusters converts the wire form and validates it as a reservation
// table, so an inconsistent push is refused before it replaces working state.
func syncedClusters(in []SyncedCluster) ([]cluster.Cluster, error) {
	clusters := make([]cluster.Cluster, 0, len(in))
	for _, item := range in {
		converted := cluster.Cluster{
			Name:        item.Name,
			SubnetIndex: item.SubnetIndex,
			Nodes:       make([]cluster.Node, 0, len(item.Nodes)),
		}
		for _, node := range item.Nodes {
			converted.Nodes = append(converted.Nodes, cluster.Node{Name: node.Name, MAC: node.MAC, IP: node.IP})
		}
		clusters = append(clusters, converted)
	}
	if _, err := cluster.NewReservationTable(clusters); err != nil {
		return nil, fmt.Errorf("validate synced reservations: %w", err)
	}
	return clusters, nil
}
