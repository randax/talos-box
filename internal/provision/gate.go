package provision

import (
	"context"
	"strings"
)

// Gate names one convergence gate: a wait inside a provisioning pass that holds
// the pass until the cluster reaches a specific observed state. Every long
// stretch a provision can stall in is one of these, and a stalled provision is
// only diagnosable if the gate that is holding it can say so — otherwise a
// twenty-minute wait is indistinguishable from a hang (#390).
type Gate string

const (
	GateDHCPLease           Gate = "DHCP lease"
	GateMachineConfig       Gate = "machine config"
	GateAPIServer           Gate = "kube-apiserver"
	GateKubernetesReady     Gate = "Kubernetes nodes Ready"
	GateChartCRDs           Gate = "chart CRDs"
	GateCiliumCRDs          Gate = "Cilium CRDs"
	GateCilium              Gate = "Cilium workloads"
	GateCiliumApply         Gate = "Cilium chart apply"
	GateHubble              Gate = "Hubble workloads"
	GateMetalLB             Gate = "MetalLB workloads"
	GateLoadBalancerVIP     Gate = "LoadBalancer VIP"
	GateLonghorn            Gate = "Longhorn workloads"
	GateLonghornScheduling  Gate = "Longhorn node scheduling"
	GateLocalPath           Gate = "local-path provisioner"
	GateStorageClass        Gate = "StorageClass replacement"
	GateDefaultStorageClass Gate = "default StorageClass"
	GateStorageProbePVC     Gate = "storage probe PVC"
	GateStorageProbePod     Gate = "storage probe pod"
	GateStorageProbeCleanup Gate = "storage probe cleanup"
)

// GateObserver is told, on every failed check of a convergence gate, which gate
// is still waiting and what it is currently blocked on. It is called from the
// polling goroutine and may be called very often — rate-limiting and
// deduplication belong to the observer, not to the gates.
type GateObserver func(gate Gate, blocker error)

type gateObserverKey struct{}

// WithGateObserver installs observer for every gate reached under ctx. It rides
// on the context rather than on Request because the gates live several layers
// below the reconcile that owns the request, and threading a reporter through
// each of them would touch every signature for one diagnostic.
func WithGateObserver(ctx context.Context, observer GateObserver) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, gateObserverKey{}, observer)
}

// reportGate hands one gate observation to whatever observer ctx carries.
func reportGate(ctx context.Context, gate Gate, blocker error) {
	if gate == "" || blocker == nil {
		return
	}
	observer, _ := ctx.Value(gateObserverKey{}).(GateObserver)
	if observer == nil {
		return
	}
	observer(gate, blocker)
}

// BlockerMessage renders one gate blocker as the single line a status surface
// or a log entry can carry: a joined multi-observation error is folded onto one
// line and a long message is cut, because the blocker is a label for what the
// gate is waiting on, not the full diagnosis.
func BlockerMessage(blocker error) string {
	if blocker == nil {
		return ""
	}
	lines := make([]string, 0, 4)
	for line := range strings.SplitSeq(blocker.Error(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	message := strings.Join(lines, "; ")
	// Cut on a rune boundary: slicing bytes can split a multi-byte rune and
	// leave invalid UTF-8 in the daemon log and in the status JSON.
	const blockerMessageMaxLen = 200
	if runes := []rune(message); len(runes) > blockerMessageMaxLen {
		message = string(runes[:blockerMessageMaxLen]) + "…"
	}
	return message
}
