package main

import (
	"strings"
	"testing"
)

func uidPtr(uid uint32) *uint32 { return &uid }

func TestRenderLaunchdPlist(t *testing.T) {
	t.Parallel()

	got, err := renderLaunchdPlist("/opt/Talos & Box/tbx-helper", uidPtr(501))
	if err != nil {
		t.Fatal(err)
	}
	const want = "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
		"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
		"<plist version=\"1.0\">\n" +
		"<dict>\n" +
		"  <key>Label</key>\n" +
		"  <string>dev.talosbox.helper</string>\n" +
		"  <key>ProgramArguments</key>\n" +
		"  <array>\n" +
		"    <string>/opt/Talos &amp; Box/tbx-helper</string>\n" +
		"    <string>--allowed-uid</string>\n" +
		"    <string>501</string>\n" +
		"  </array>\n" +
		"  <key>RunAtLoad</key>\n" +
		"  <true/>\n" +
		"  <key>KeepAlive</key>\n" +
		"  <true/>\n" +
		"</dict>\n" +
		"</plist>\n"
	if string(got) != want {
		t.Fatalf("plist:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderLaunchdPlistWithoutAllowedUID(t *testing.T) {
	t.Parallel()

	got, err := renderLaunchdPlist("/usr/local/bin/tbx-helper", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "--allowed-uid") {
		t.Fatalf("plist includes --allowed-uid without an allowed uid:\n%s", got)
	}
	if !strings.Contains(string(got), "<string>/usr/local/bin/tbx-helper</string>\n  </array>") {
		t.Fatalf("plist program arguments malformed:\n%s", got)
	}
}

func TestRenderLaunchdPlistRejectsRelativePath(t *testing.T) {
	t.Parallel()

	if _, err := renderLaunchdPlist("tbx-helper", uidPtr(501)); err == nil {
		t.Fatal("renderLaunchdPlist accepted a relative path")
	}
}

func TestAllowedUIDFromSudoEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		value     string
		present   bool
		want      *uint32
		wantError string
	}{
		{name: "user uid", value: "501", present: true, want: uidPtr(501)},
		{name: "root uid", value: "0", present: true, want: uidPtr(0)},
		{name: "unset means root-only", want: nil},
		{name: "empty means root-only", present: true, want: nil},
		{name: "negative", value: "-1", present: true, wantError: "invalid SUDO_UID"},
		{name: "not numeric", value: "user", present: true, wantError: "invalid SUDO_UID"},
		{name: "too large", value: "4294967296", present: true, wantError: "invalid SUDO_UID"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := allowedUIDFromSudoEnv(func(string) (string, bool) {
				return test.value, test.present
			})
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want containing %q", err, test.wantError)
				}
				if !strings.Contains(err.Error(), "sudo tbx system install") {
					t.Fatalf("error = %v, want reinstall guidance", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case test.want == nil && got != nil:
				t.Fatalf("allowed uid = %d, want nil", *got)
			case test.want != nil && got == nil:
				t.Fatalf("allowed uid = nil, want %d", *test.want)
			case test.want != nil && *got != *test.want:
				t.Fatalf("allowed uid = %d, want %d", *got, *test.want)
			}
		})
	}
}
