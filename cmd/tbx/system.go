package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	helperLaunchdLabel = "dev.talosbox.helper"
	helperPlistPath    = "/Library/LaunchDaemons/dev.talosbox.helper.plist"
)

func (c cli) runSystem(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tbx system install|uninstall")
	}
	switch args[0] {
	case "install":
		if len(args) > 2 {
			return errors.New("usage: tbx system install [absolute-helper-path]")
		}
		if len(args) == 2 && !filepath.IsAbs(args[1]) {
			return errors.New("tbx-helper path must be absolute")
		}
	case "uninstall":
		if len(args) != 1 {
			return errors.New("usage: tbx system uninstall")
		}
	default:
		return fmt.Errorf("unknown system command %q", args[0])
	}

	if os.Geteuid() != 0 {
		if _, err := fmt.Fprintf(c.err, "tbx system %s requires root; re-running with sudo\n", args[0]); err != nil {
			return err
		}
		return reexecSystemWithSudo(args)
	}
	if args[0] == "uninstall" {
		return c.uninstallSystem()
	}
	return c.installSystem(args[1:])
}

func (c cli) installSystem(args []string) error {
	helperPath, err := resolveHelperPath(args)
	if err != nil {
		return err
	}
	if err := validateHelperBinary(helperPath); err != nil {
		return err
	}
	allowedUID, err := allowedUIDFromSudoEnv(os.LookupEnv)
	if err != nil {
		return err
	}
	if allowedUID == nil {
		if _, err := fmt.Fprintln(c.err, "warning: SUDO_UID is not set; only root will be able to use tbx-helper; re-run `sudo tbx system install` from your account to authorize it"); err != nil {
			return err
		}
	}
	plist, err := renderLaunchdPlist(helperPath, allowedUID)
	if err != nil {
		return err
	}

	if _, err := os.Stat(helperPlistPath); err == nil {
		_ = runLaunchctl("bootout", "system", helperPlistPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect helper plist: %w", err)
	}
	if err := os.WriteFile(helperPlistPath, plist, 0o644); err != nil {
		return fmt.Errorf("write helper plist: %w", err)
	}
	if err := os.Chmod(helperPlistPath, 0o644); err != nil {
		return fmt.Errorf("set helper plist permissions: %w", err)
	}
	if err := os.Chown(helperPlistPath, 0, 0); err != nil {
		return fmt.Errorf("set helper plist ownership: %w", err)
	}
	if err := runLaunchctl("bootstrap", "system", helperPlistPath); err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.out, "installed %s using %s\n", helperLaunchdLabel, helperPath)
	return err
}

func (c cli) uninstallSystem() error {
	if _, err := os.Stat(helperPlistPath); err == nil {
		if err := runLaunchctl("bootout", "system", helperPlistPath); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect helper plist: %w", err)
	} else {
		// A loaded job can outlive a manually removed plist.
		_ = runLaunchctl("bootout", "system/"+helperLaunchdLabel)
	}
	if err := os.Remove(helperPlistPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove helper plist: %w", err)
	}
	_, err := fmt.Fprintln(c.out, "uninstalled "+helperLaunchdLabel)
	return err
}

func resolveHelperPath(args []string) (string, error) {
	if len(args) == 1 {
		if !filepath.IsAbs(args[0]) {
			return "", errors.New("tbx-helper path must be absolute")
		}
		return filepath.Clean(args[0]), nil
	}
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find tbx executable: %w", err)
	}
	// Joining from the executable itself with ../ resolves to its sibling.
	return filepath.Clean(filepath.Join(executable, "..", "tbx-helper")), nil
}

func validateHelperBinary(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect tbx-helper binary: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("tbx-helper path is not a regular file: %s", path)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("tbx-helper path is not executable: %s", path)
	}
	return nil
}

func renderLaunchdPlist(helperPath string, allowedUID *uint32) ([]byte, error) {
	if !filepath.IsAbs(helperPath) {
		return nil, errors.New("tbx-helper path must be absolute")
	}
	var escaped bytes.Buffer
	if err := xml.EscapeText(&escaped, []byte(helperPath)); err != nil {
		return nil, fmt.Errorf("escape helper path: %w", err)
	}
	uidArgs := ""
	if allowedUID != nil {
		uidArgs = fmt.Sprintf("    <string>--allowed-uid</string>\n    <string>%d</string>\n", *allowedUID)
	}
	const template = "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
		"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
		"<plist version=\"1.0\">\n" +
		"<dict>\n" +
		"  <key>Label</key>\n" +
		"  <string>%s</string>\n" +
		"  <key>ProgramArguments</key>\n" +
		"  <array>\n" +
		"    <string>%s</string>\n" +
		"%s" +
		"  </array>\n" +
		"  <key>RunAtLoad</key>\n" +
		"  <true/>\n" +
		"  <key>KeepAlive</key>\n" +
		"  <true/>\n" +
		"</dict>\n" +
		"</plist>\n"
	return []byte(fmt.Sprintf(template, helperLaunchdLabel, escaped.String(), uidArgs)), nil
}

// allowedUIDFromSudoEnv returns nil when SUDO_UID is absent (e.g. a direct
// root shell), which installs the helper in root-only mode.
func allowedUIDFromSudoEnv(lookupEnv func(string) (string, bool)) (*uint32, error) {
	value, ok := lookupEnv("SUDO_UID")
	if !ok || value == "" {
		return nil, nil
	}
	uid, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid SUDO_UID %q; run `sudo tbx system install` from your own account: %w", value, err)
	}
	allowed := uint32(uid)
	return &allowed, nil
}

func reexecSystemWithSudo(args []string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find tbx executable: %w", err)
	}
	commandArgs := append([]string{executable, "system"}, args...)
	command := exec.Command("sudo", commandArgs...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("run sudo: %w", err)
	}
	return nil
}

func runLaunchctl(args ...string) error {
	output, err := exec.Command("/bin/launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}
