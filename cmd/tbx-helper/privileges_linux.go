//go:build linux

package main

func requirePrivileges() error { return nil }

func warnMissingAllowedUID(*uint32) {}
