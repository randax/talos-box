package main

import "testing"

func TestRenderLaunchdPlist(t *testing.T) {
	t.Parallel()

	got, err := renderLaunchdPlist("/opt/Talos & Box/tbx-helper")
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

	if _, err := renderLaunchdPlist("tbx-helper"); err == nil {
		t.Fatal("renderLaunchdPlist accepted a relative path")
	}
}
