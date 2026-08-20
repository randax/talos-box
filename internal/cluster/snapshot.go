package cluster

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// snapshotNameRe forbids path separators, dots-only names, and traversal.
var snapshotNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// SnapshotInfo describes one stored snapshot. Cluster is filled only by a
// listing that spans clusters, where the snapshot name alone does not say
// which cluster it belongs to (#417).
type SnapshotInfo struct {
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
	Cluster string    `json:"cluster,omitempty"`
}

func snapshotsDir(clusterName string) (string, error) {
	dir, err := Dir(clusterName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "snapshots"), nil
}

func snapshotDir(clusterName, name string) (string, error) {
	base, err := snapshotsDir(clusterName)
	if err != nil {
		return "", err
	}
	if !snapshotNameRe.MatchString(name) || name == "." || name == ".." {
		return "", fmt.Errorf("invalid snapshot name %q (letters, digits, . _ - only; no path separators)", name)
	}
	return filepath.Join(base, name), nil
}

// CreateSnapshot clones every node disk and the cluster state into a named
// snapshot — one crash-consistent set (the caller stops the VMs first).
func CreateSnapshot(item Cluster, name string) error {
	dest, err := snapshotDir(item.Name, name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("snapshot %q already exists", name)
	}
	live, err := Dir(item.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	// build in a temp dir, then rename into place so a partial snapshot is
	// never visible to list/restore
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".snap-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	for _, node := range item.Nodes {
		if err := cloneOrCopy(filepath.Join(live, node.Name+".img"), filepath.Join(tmp, node.Name+".img")); err != nil {
			return fmt.Errorf("snapshot node %s: %w", node.Name, err)
		}
	}
	if err := copyFile(filepath.Join(live, stateFile), filepath.Join(tmp, stateFile)); err != nil {
		return fmt.Errorf("snapshot cluster state: %w", err)
	}
	return os.Rename(tmp, dest)
}

// RestoreSnapshot clones a snapshot's disks back over the live ones and
// restores the cluster state (the caller stops the VMs first, cold-boots after).
func RestoreSnapshot(item Cluster, name string) error {
	src, err := snapshotDir(item.Name, name)
	if err != nil {
		return err
	}
	// validate the captured state and its disks before touching a single file:
	// the restore deletes live disks and the live state file, and only then
	// installs the snapshot's — a defect discovered at that point would have
	// already destroyed the cluster it was supposed to restore
	captured, err := readSnapshotState(item.Name, name)
	if err != nil {
		return err
	}
	if err := checkSnapshotDisks(src, name, captured.Nodes); err != nil {
		return err
	}
	live, err := Dir(item.Name)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	// stage every snapshot disk as a temp beside its target; only swap once all
	// clones succeed, so a mid-way failure leaves the live disks untouched
	snapImgs := map[string]bool{}
	var staged [][2]string // {temp, target}
	cleanup := func() {
		for _, pair := range staged {
			_ = os.Remove(pair[0])
		}
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".img" {
			continue
		}
		snapImgs[entry.Name()] = true
		target := filepath.Join(live, entry.Name())
		temp := target + ".restoring"
		if err := cloneOrCopy(filepath.Join(src, entry.Name()), temp); err != nil {
			cleanup()
			return fmt.Errorf("restore %s: %w", entry.Name(), err)
		}
		staged = append(staged, [2]string{temp, target})
	}
	for _, pair := range staged {
		if err := os.Rename(pair[0], pair[1]); err != nil {
			cleanup()
			return fmt.Errorf("swap %s: %w", filepath.Base(pair[1]), err)
		}
	}
	// remove live disks for nodes the snapshot did not capture
	liveEntries, _ := os.ReadDir(live)
	for _, entry := range liveEntries {
		if filepath.Ext(entry.Name()) == ".img" && !snapImgs[entry.Name()] {
			_ = os.Remove(filepath.Join(live, entry.Name()))
		}
	}
	// restore the exact node set the snapshot captured (overwrite live state)
	liveState := filepath.Join(live, stateFile)
	if err := os.Remove(liveState); err != nil && !os.IsNotExist(err) {
		return err
	}
	return copyFile(filepath.Join(src, stateFile), liveState)
}

// SnapshotNodes returns the node set the named snapshot captured, so a caller
// can tell which live nodes a restore would delete.
func SnapshotNodes(clusterName, name string) ([]Node, error) {
	captured, err := readSnapshotState(clusterName, name)
	if err != nil {
		return nil, err
	}
	return captured.Nodes, nil
}

// checkSnapshotDisks rejects a snapshot whose captured state and disk images
// disagree. The restore deletes live disks by which images the snapshot dir
// holds, while callers reason about which nodes it deletes from the captured
// state: a node listed without its image would have its live disk deleted
// ungated, and a stray image would restore a disk for a node no state claims.
func checkSnapshotDisks(dir, name string, nodes []Node) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	images := map[string]bool{}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".img" {
			images[entry.Name()] = true
		}
	}
	for _, node := range nodes {
		image := node.Name + ".img"
		if !images[image] {
			return fmt.Errorf("snapshot %q is missing the disk image for node %q", name, node.Name)
		}
		delete(images, image)
	}
	stray := make([]string, 0, len(images))
	for image := range images {
		stray = append(stray, image)
	}
	if len(stray) > 0 {
		sort.Strings(stray)
		return fmt.Errorf("snapshot %q has disk image %q for no captured node", name, stray[0])
	}
	return nil
}

func readSnapshotState(clusterName, name string) (Cluster, error) {
	dir, err := snapshotDir(clusterName, name)
	if err != nil {
		return Cluster{}, err
	}
	if _, err := os.Stat(dir); err != nil {
		return Cluster{}, fmt.Errorf("snapshot %q does not exist", name)
	}
	data, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		return Cluster{}, fmt.Errorf("snapshot %q has no readable cluster state: %w", name, err)
	}
	var captured Cluster
	if err := json.Unmarshal(data, &captured); err != nil {
		return Cluster{}, fmt.Errorf("snapshot %q has unreadable cluster state: %w", name, err)
	}
	return captured, nil
}

// ListSnapshots returns the cluster's snapshots, newest first. The result is
// never nil on success: every daemon response embedding it must marshal an
// empty set as [] — one wire contract across list, delete, create, restore.
func ListSnapshots(clusterName string) ([]SnapshotInfo, error) {
	out := []SnapshotInfo{}
	base, err := snapshotsDir(clusterName)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name()[0] == '.' {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		out = append(out, SnapshotInfo{Name: entry.Name(), Created: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// DeleteSnapshot removes a named snapshot.
func DeleteSnapshot(clusterName, name string) error {
	dir, err := snapshotDir(clusterName, name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return fmt.Errorf("snapshot %q does not exist", name)
	}
	return os.RemoveAll(dir)
}

// cloneOrCopy clones a file (APFS copy-on-write) or falls back to a byte copy.
func cloneOrCopy(source, destination string) error {
	if err := cloneFile(source, destination); err == nil {
		return nil
	}
	return copyFile(source, destination)
}
