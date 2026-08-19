//go:build !darwin

package daemon

import "github.com/randax/talos-box/internal/hostport"

// bgpPortListeners has no inventory outside macOS yet: Linux already carries a
// port-179 doctor check built on `ss`, and reporting nothing is honest where
// nothing was measured. A nil inventory makes bgpPortSquatterWarning silent
// (#359).
var bgpPortListeners func() ([]hostport.Listener, error)
