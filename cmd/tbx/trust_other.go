//go:build !darwin && !linux

package main

import "runtime"

func nativeTrustGOOS() string { return runtime.GOOS }
