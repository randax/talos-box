package cluster

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// snapshotNameRe forbids path separators, dots-only names, and traversal.
var snapshotNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// SnapshotInfo describes one stored snapshot.
type SnapshotInfo struct {
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
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
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("snapshot %q does not exist", name)
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

// ListSnapshots returns the cluster's snapshots, newest first.
func ListSnapshots(clusterName string) ([]SnapshotInfo, error) {
	base, err := snapshotsDir(clusterName)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []SnapshotInfo
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
