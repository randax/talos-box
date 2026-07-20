package cluster

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func ProvisionDisks(c Cluster, cachedDisk string) error {
	if c.NodeDefaults.DiskGiB <= 0 || int64(c.NodeDefaults.DiskGiB) > int64(^uint64(0)>>1)/(1<<30) {
		return fmt.Errorf("invalid disk size %d GiB", c.NodeDefaults.DiskGiB)
	}
	dir, err := Dir(c.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create cluster directory: %w", err)
	}

	for _, node := range c.Nodes {
		if err := validName(node.Name); err != nil {
			return fmt.Errorf("invalid node name %q: %w", node.Name, err)
		}
		if err := provisionDisk(cachedDisk, filepath.Join(dir, node.Name+".img"), int64(c.NodeDefaults.DiskGiB)<<30); err != nil {
			return fmt.Errorf("provision disk for %s: %w", node.Name, err)
		}
	}
	return nil
}

func provisionDisk(source, destination string, size int64) error {
	if info, err := os.Stat(destination); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("existing disk is not a regular file")
		}
		if info.Size() < size {
			return os.Truncate(destination, size)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(destination), ".disk-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Remove(tmpName); err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmpName) }()

	if err := cloneFile(source, tmpName); err != nil {
		if removeErr := os.Remove(tmpName); removeErr != nil && !os.IsNotExist(removeErr) {
			return removeErr
		}
		if err := copyFile(source, tmpName); err != nil {
			return err
		}
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Truncate(tmpName, size); err != nil {
		return err
	}
	if err := os.Rename(tmpName, destination); err != nil {
		return err
	}
	return nil
}

func copyFile(source, destination string) error {
	src, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	dst, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	return dst.Close()
}
