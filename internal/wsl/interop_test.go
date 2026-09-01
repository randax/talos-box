package wsl

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestWindowsInteropReadsBuildWithOneRegQuery(t *testing.T) {
	t.Parallel()

	var calls [][]string
	interop := NewWindowsInterop(func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return []byte("\r\nHKEY_LOCAL_MACHINE\\SOFTWARE\\Microsoft\\Windows NT\\CurrentVersion\r\n    CurrentBuildNumber    REG_SZ    26100\r\n"), nil
	})
	build, err := interop.WindowsBuild()
	if err != nil {
		t.Fatal(err)
	}
	if build != "26100" {
		t.Fatalf("build = %q, want 26100", build)
	}
	want := [][]string{{"reg.exe", "query", `HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion`, "/v", "CurrentBuildNumber"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestWindowsInteropRejectsMissingOrMalformedBuildValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		output []byte
		err    error
	}{
		{name: "interop disabled", err: errors.New("exec format error")},
		{name: "empty output"},
		{name: "wrong value name", output: []byte("CurrentBuild REG_SZ 26100")},
		{name: "value with no data", output: []byte("CurrentBuildNumber REG_SZ")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			interop := NewWindowsInterop(func(string, ...string) ([]byte, error) {
				calls++
				return tt.output, tt.err
			})
			if build, err := interop.WindowsBuild(); err == nil {
				t.Fatalf("WindowsBuild() = %q, nil; want error", build)
			}
			if calls != 1 {
				t.Fatalf("calls = %d, want 1", calls)
			}
		})
	}
}

func ExampleWindowsInterop_WindowsBuild() {
	interop := NewWindowsInterop(func(string, ...string) ([]byte, error) {
		return []byte("CurrentBuildNumber REG_SZ 26100"), nil
	})
	build, _ := interop.WindowsBuild()
	fmt.Println(build)
	// Output: 26100
}
