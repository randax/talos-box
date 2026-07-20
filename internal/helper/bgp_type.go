package helper

// bgpSpeaker is the platform-neutral handle the server keeps per cluster; the
// concrete type is the darwin bgp.Speaker.
type bgpSpeaker interface {
	Stop()
}
