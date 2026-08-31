package main

// Platform algorithms live in trust_platform.go so injected-GOOS unit tests
// exercise every host driver from one test binary. The native default remains
// build-selected here.
func nativeTrustGOOS() string { return "darwin" }
