//go:build linux

package helper

import "testing"

func TestLinuxRejectsResolverFileInstallation(t *testing.T) {
	t.Parallel()
	if err := installHostResolver(53); err == nil {
		t.Fatal("Linux accepted resolver-file installation")
	}
}
