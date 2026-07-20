package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

const stateFile = "cluster.json"

func Dir(name string) (string, error) {
	if err := validName(name); err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".talosbox", "clusters", name), nil
}

func Save(c Cluster) error {
	dir, err := Dir(c.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create cluster directory: %w", err)
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode cluster state: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".cluster.json-*")
	if err != nil {
		return fmt.Errorf("create temporary cluster state: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set cluster state permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write cluster state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close cluster state: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, stateFile)); err != nil {
		return fmt.Errorf("install cluster state: %w", err)
	}
	return nil
}

func Load(name string) (Cluster, error) {
	dir, err := Dir(name)
	if err != nil {
		return Cluster{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		return Cluster{}, fmt.Errorf("read cluster state: %w", err)
	}
	var c Cluster
	if err := json.Unmarshal(data, &c); err != nil {
		return Cluster{}, fmt.Errorf("decode cluster state: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err == nil && fields["subnetIndex"] == nil {
		c.SubnetIndex = c.Index
	}
	return c, nil
}

// List returns all persisted clusters ordered by name.
func List() ([]Cluster, error) {
	root, err := clustersDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}

	clusters := make([]Cluster, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		item, err := Load(entry.Name())
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		clusters = append(clusters, item)
	}
	sort.Slice(clusters, func(i, j int) bool { return clusters[i].Name < clusters[j].Name })
	return clusters, nil
}

// Destroy removes all persisted state for name.
func Destroy(name string) error {
	dir, err := Dir(name)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("destroy cluster: %w", err)
	}
	return nil
}

func clustersDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".talosbox", "clusters"), nil
}

func validName(name string) error {
	if name == "" {
		return errors.New("cluster name cannot be empty")
	}
	if filepath.Base(name) != name || name == "." || name == ".." {
		return fmt.Errorf("invalid cluster name %q", name)
	}
	return nil
}
