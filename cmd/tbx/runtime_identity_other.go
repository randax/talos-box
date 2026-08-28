//go:build !linux

package main

import (
	"os"
	"runtime"
	"strings"
)

func runtimeIdentityPlatformDeps(deps *runtimeIdentityDeps) {
	if deps == nil {
		return
	}
	if deps.readFile == nil {
		deps.readFile = os.ReadFile
	}
	if deps.helperConfiguredPath == nil && deps.helperProbe == nil {
		deps.helperConfiguredPath = func() configuredComponentPath {
			return helperConfiguredPathForCurrentPlatform(deps.readFile)
		}
	}
	if deps.daemonProbe == nil {
		deps.daemonProbe = deps.daemonDial
	}
	if deps.helperProbe == nil {
		deps.helperProbe = deps.helperDial
	}
}

func helperConfiguredPathForCurrentPlatform(readFile func(string) ([]byte, error)) configuredComponentPath {
	if runtime.GOOS != "darwin" {
		return configuredComponentPath{}
	}
	return launchdConfiguredPath(readFile, helperPlistPath)
}

func launchdConfiguredPath(readFile func(string) ([]byte, error), path string) configuredComponentPath {
	if readFile == nil {
		readFile = os.ReadFile
	}
	data, err := readFile(path)
	if err != nil {
		return configuredComponentPath{}
	}
	text := string(data)
	for _, key := range []string{"ProgramArguments", "Program"} {
		index := strings.Index(text, "<key>"+key+"</key>")
		if index < 0 {
			continue
		}
		rest := text[index:]
		start := strings.Index(rest, "<string>")
		end := strings.Index(rest, "</string>")
		if start < 0 || end < 0 || end <= start+len("<string>") {
			continue
		}
		return configuredComponentPath{
			Path:   rest[start+len("<string>") : end],
			Source: "launchd",
		}
	}
	return configuredComponentPath{}
}
