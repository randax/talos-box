package main

import (
	"strings"
	"testing"
)

func TestRenderLaunchdPlist(t *testing.T) {
	t.Parallel()

	got, err := renderLaunchdPlist("/opt/Talos & Box/tbx-helper", 501)
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

func TestRenderLaunchdPlistRejectsRelativePath(t *testing.T) {
	t.Parallel()

	if _, err := renderLaunchdPlist("tbx-helper", 501); err == nil {
		t.Fatal("renderLaunchdPlist accepted a relative path")
	}
}

func TestAllowedUIDFromSudoEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		value     string
		present   bool
		want      uint32
		wantError string
	}{
		{name: "user uid", value: "501", present: true, want: 501},
		{name: "root uid", value: "0", present: true, want: 0},
		{name: "unset", wantError: "SUDO_UID is not set"},
		{name: "empty", present: true, wantError: "SUDO_UID is not set"},
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
				if err == nil || !strings.Contains(err.Error(), "sudo tbx system install") {
					t.Fatalf("error = %v, want reinstall guidance", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("allowed uid = %d, want %d", got, test.want)
			}
		})
	}
}
