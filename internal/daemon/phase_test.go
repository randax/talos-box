package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
)

func TestClassifyPhase(t *testing.T) {
	tests := []struct {
		name    string
		running bool
		probe   ProbeResult
		want    Phase
	}{
		{"vm stopped", false, ProbeResult{}, PhaseStopped},
		{"running, no answer on apid", true, ProbeResult{Dialed: false}, PhaseUnreachable},
		{"running, apid not speaking TLS", true, ProbeResult{Dialed: true, TLS: false}, PhaseUnreachable},
		{"running, cluster-CA cert", true, ProbeResult{Dialed: true, TLS: true}, PhaseConfigured},
		{"running, maintenance cert", true, ProbeResult{Dialed: true, TLS: true, MaintenanceCert: true}, PhaseMaintenance},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyPhase(tt.running, tt.probe); got != tt.want {
				t.Errorf("ClassifyPhase(%v, %+v) = %q, want %q", tt.running, tt.probe, got, tt.want)
			}
		})
	}
}

func TestHints(t *testing.T) {
	base := ClusterStatus{Name: "demo", Subnet: "172.30.0.0/24"}
	node := func(name string, phase Phase) NodeStatus {
		return NodeStatus{Name: name, Phase: phase, IP: "172.30.0.2"}
	}
	tests := []struct {
		name  string
		nodes []NodeStatus
		want  []string // substrings that must each appear in exactly the hint list
	}{
		{
			name:  "maintenance node suggests config workflow",
			nodes: []NodeStatus{node("demo-cp-1", PhaseMaintenance)},
			want:  []string{"talosctl gen config", "apply-config --insecure"},
		},
		{
			name:  "all configured suggests bootstrap and the dashboard",
			nodes: []NodeStatus{node("demo-cp-1", PhaseConfigured)},
			want: []string{
				"talosctl bootstrap --talosconfig ./talosconfig --nodes 172.30.0.2 --endpoints 172.30.0.2",
				"talosctl kubeconfig . --talosconfig ./talosconfig --nodes 172.30.0.2 --endpoints 172.30.0.2",
				"talosctl dashboard --talosconfig ./talosconfig --nodes 172.30.0.2 --endpoints 172.30.0.2",
			},
		},
		{
			name:  "stopped cluster suggests start",
			nodes: []NodeStatus{node("demo-cp-1", PhaseStopped)},
			want:  []string{"tbx cluster start demo"},
		},
		{
			name:  "unreachable suggests patience then doctor",
			nodes: []NodeStatus{node("demo-cp-1", PhaseUnreachable)},
			want:  []string{"tbx doctor"},
		},
		{
			name: "mixed phases yield maintenance hint, not bootstrap",
			nodes: []NodeStatus{
				node("demo-cp-1", PhaseConfigured),
				node("demo-worker-1", PhaseMaintenance),
			},
			want: []string{"apply-config --insecure"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := base
			status.Nodes = tt.nodes
			hints := Hints(status)
			for _, substr := range tt.want {
				found := false
				for _, h := range hints {
					if strings.Contains(h, substr) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("hints %q missing substring %q", hints, substr)
				}
			}
		})
	}
	// the gen-config endpoint must name a control plane, not the maintenance worker
	status2 := base
	status2.Nodes = []NodeStatus{
		{Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2"},
		{Name: "demo-worker-1", Role: cluster.RoleWorker, Phase: PhaseMaintenance, IP: "172.30.0.3"},
	}
	genHintFound := false
	for _, h := range Hints(status2) {
		if strings.Contains(h, "gen config") {
			genHintFound = true
			if !strings.Contains(h, "demo-cp-1.demo.k8s.test") {
				t.Errorf("gen config hint should use the control-plane endpoint, got %q", h)
			}
		}
	}
	if !genHintFound {
		t.Error("expected a gen config hint for the maintenance worker")
	}

	// bootstrap hint must NOT appear while any node is in maintenance
	status := base
	status.Nodes = []NodeStatus{node("a", PhaseConfigured), node("b", PhaseMaintenance)}
	for _, h := range Hints(status) {
		if strings.Contains(h, "bootstrap") {
			t.Errorf("bootstrap hint offered while a node is still in maintenance: %q", h)
		}
	}
}

func TestHintsDescribeFlannelReadyWithoutLoadBalancer(t *testing.T) {
	status := ClusterStatus{
		Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel}, KubernetesReady: true,
		Nodes: []NodeStatus{{Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2"}},
	}
	hints := Hints(status)
	joined := strings.Join(hints, "\n")
	for _, wanted := range []string{"Ready", "lb: false", "disabled", "TALOSCONFIG=~/.talosbox/clusters/demo/talosconfig", "KUBECONFIG=~/.talosbox/clusters/demo/kubeconfig"} {
		if !strings.Contains(joined, wanted) {
			t.Fatalf("hints missing %q:\n%s", wanted, joined)
		}
	}
}

func TestHintsAccumulateStorageProvisioningAndFlannelReadyWithoutLoadBalancer(t *testing.T) {
	status := ClusterStatus{
		Name:               "demo",
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILocalPath},
		KubernetesReady:    true,
		StoragePhase:       StoragePhaseProvisioning,
		Nodes: []NodeStatus{{
			Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2",
		}},
	}

	hints := Hints(status)
	joined := strings.Join(hints, "\n")
	for _, wanted := range []string{
		"storage provisioning",
		"waiting for the CSI readiness probe to pass",
		"Kubernetes is Ready with Talos-managed flannel",
	} {
		if !strings.Contains(joined, wanted) {
			t.Fatalf("hints missing %q:\n%s", wanted, joined)
		}
	}
}

func TestHintsDescribeLiveFlannelMetalLBVIPAndPolicyLimit(t *testing.T) {
	status := ClusterStatus{
		Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true}, KubernetesReady: true,
		VIP: "172.30.4.200", VIPLive: true,
		Nodes: []NodeStatus{{Role: cluster.RoleControlPlane, Phase: PhaseConfigured}},
	}
	hints := Hints(status)
	if len(hints) != 1 || !strings.Contains(hints[0], "http://172.30.4.200/") || !strings.Contains(hints[0], "does not enforce NetworkPolicies") {
		t.Fatalf("hints = %v", hints)
	}
}

func TestHintsDescribeStorageLive(t *testing.T) {
	status := ClusterStatus{
		Name:               "demo",
		ProvisioningIntent: cluster.ProvisioningIntent{CSI: cluster.CSILocalPath},
		StoragePhase:       StoragePhaseLive,
	}

	hints := Hints(status)
	if len(hints) != 1 || !strings.Contains(hints[0], "storage live") || !strings.Contains(hints[0], "CSI readiness probe passed") {
		t.Fatalf("hints = %v", hints)
	}
}

func TestHintsKeepSingleNodeLonghornWarningAlongsideNetworkingHints(t *testing.T) {
	status := ClusterStatus{
		Name:               "demo",
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILonghorn, LB: true},
		Running:            true,
		KubernetesReady:    true,
		StoragePhase:       StoragePhaseLive,
		VIP:                "172.30.0.200",
		VIPLive:            true,
		Nodes: []NodeStatus{{
			Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2",
		}},
	}

	joined := strings.Join(Hints(status), "\n")
	for _, wanted := range []string{
		"storage live",
		"no redundancy",
		"http://172.30.0.200/",
	} {
		if !strings.Contains(joined, wanted) {
			t.Fatalf("hints missing %q:\n%s", wanted, joined)
		}
	}
}

func TestHintsReportLonghornRedundancyByStorageNodeCount(t *testing.T) {
	controlPlane := NodeStatus{Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2"}
	worker1 := NodeStatus{Name: "demo-worker-1", Role: cluster.RoleWorker, Phase: PhaseConfigured, IP: "172.30.0.3"}
	worker2 := NodeStatus{Name: "demo-worker-2", Role: cluster.RoleWorker, Phase: PhaseConfigured, IP: "172.30.0.4"}
	tests := []struct {
		name  string
		nodes []NodeStatus
		want  bool
	}{
		{name: "one worker holds the only replica", nodes: []NodeStatus{controlPlane, worker1}, want: true},
		{name: "two workers replicate", nodes: []NodeStatus{controlPlane, worker1, worker2}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := ClusterStatus{
				Name:               "demo",
				ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILonghorn},
				Running:            true,
				KubernetesReady:    true,
				StoragePhase:       StoragePhaseLive,
				Nodes:              tt.nodes,
			}
			got := strings.Contains(strings.Join(Hints(status), "\n"), "no redundancy")
			if got != tt.want {
				t.Fatalf("no-redundancy hint present = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHintsSuppressSingleNodeLonghornWarningUntilStorageIsLive(t *testing.T) {
	node := NodeStatus{
		Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2",
	}
	tests := []struct {
		name   string
		status ClusterStatus
	}{
		{
			name: "stopped cluster",
			status: ClusterStatus{
				Name:               "demo",
				ProvisioningIntent: cluster.ProvisioningIntent{CSI: cluster.CSILonghorn},
				StoragePhase:       StoragePhaseLive,
				Nodes:              []NodeStatus{node},
			},
		},
		{
			name: "storage still provisioning",
			status: ClusterStatus{
				Name:               "demo",
				ProvisioningIntent: cluster.ProvisioningIntent{CSI: cluster.CSILonghorn},
				Running:            true,
				StoragePhase:       StoragePhaseProvisioning,
				Nodes:              []NodeStatus{node},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, hint := range Hints(tt.status) {
				if strings.Contains(hint, "no redundancy") {
					t.Fatalf("Hints() unexpectedly reported live single-node Longhorn warning: %q", hint)
				}
			}
		})
	}
}

func TestHintsReportCiliumStorageProvisioning(t *testing.T) {
	status := ClusterStatus{
		Name:               "demo",
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, CSI: cluster.CSILonghorn},
		StoragePhase:       StoragePhaseProvisioning,
	}

	joined := strings.Join(Hints(status), "\n")
	if !strings.Contains(joined, "storage provisioning") || strings.Contains(joined, "not implemented") {
		t.Fatalf("hints = %q, want active Cilium storage provisioning", joined)
	}
}

func TestHintsDescribeCiliumReadyWithAndWithoutLoadBalancer(t *testing.T) {
	for _, test := range []struct {
		name   string
		intent cluster.ProvisioningIntent
		vip    string
		live   bool
		wants  []string
	}{
		{
			name:   "live VIP",
			intent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true},
			vip:    "172.30.4.200",
			live:   true,
			wants:  []string{"Cilium LB-IPAM", "http://172.30.4.200/"},
		},
		{
			name:   "load balancer disabled",
			intent: cluster.ProvisioningIntent{CNI: cluster.CNICilium},
			wants:  []string{"Ready", "lb: false", "no VIP"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			status := ClusterStatus{
				Name:               "demo",
				ProvisioningIntent: test.intent,
				KubernetesReady:    true,
				VIP:                test.vip,
				VIPLive:            test.live,
				Nodes:              []NodeStatus{{Role: cluster.RoleControlPlane, Phase: PhaseConfigured}},
			}
			joined := strings.Join(Hints(status), "\n")
			for _, wanted := range test.wants {
				if !strings.Contains(joined, wanted) {
					t.Fatalf("hints missing %q:\n%s", wanted, joined)
				}
			}
		})
	}
}

func TestHintsDoNotInferFlannelKubernetesReadiness(t *testing.T) {
	status := ClusterStatus{
		Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel},
		Nodes: []NodeStatus{{Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2"}},
	}
	for _, hint := range Hints(status) {
		if strings.Contains(hint, "Kubernetes is Ready") {
			t.Fatalf("Hints() inferred Kubernetes readiness from Talos state: %q", hint)
		}
	}
}

func TestHintsDescribeProvisioningInProgressAndExports(t *testing.T) {
	status := ClusterStatus{
		Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true},
		ConfigOrigin: cluster.OriginManaged,
		Nodes:        []NodeStatus{{Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured}},
	}
	joined := strings.Join(Hints(status), "\n")
	for _, want := range []string{"provisioning is in progress", "tbx up; export TALOSCONFIG=~/.talosbox/clusters/demo/talosconfig", "KUBECONFIG=~/.talosbox/clusters/demo/kubeconfig"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("provisioning hint missing %q:\n%s", want, joined)
		}
	}
}

// An imperatively created cluster has no talosbox.yaml, so the recovery hint
// must not point at `tbx up`, which would refuse for want of a file (#267).
func TestHintsRecoverImperativeProvisioningWithDestroyAndRecreate(t *testing.T) {
	status := ClusterStatus{
		Name: "qa-cil", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, CSI: cluster.CSILonghorn, LB: true},
		ConfigOrigin: cluster.OriginImperative,
		Nodes:        []NodeStatus{{Name: "qa-cil-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured}},
	}
	joined := strings.Join(Hints(status), "\n")
	for _, want := range []string{
		"No talosbox.yaml backs this cluster",
		"tbx cluster destroy qa-cil --force",
		"--cni cilium --csi longhorn",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("provisioning hint missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "Rerun: tbx up") {
		t.Fatalf("provisioning hint offers tbx up for an imperative cluster:\n%s", joined)
	}
}

func TestCredentialExportsQuoteClusterName(t *testing.T) {
	got := credentialExports("demo; echo owned")
	for _, want := range []string{
		"TALOSCONFIG=~/.talosbox/clusters/'demo; echo owned'/talosconfig",
		"KUBECONFIG=~/.talosbox/clusters/'demo; echo owned'/kubeconfig",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("credentialExports() missing %q: %s", want, got)
		}
	}
}

// The calm unreachable hint states the boot budget in prose; formatting it
// from the constant keeps the promise and the clock from desyncing.
func TestFormatBootWindowRendersTheConstant(t *testing.T) {
	cases := map[time.Duration]string{
		time.Minute:      "1 minute",
		2 * time.Minute:  "2 minutes",
		90 * time.Second: "90 seconds",
	}
	for window, want := range cases {
		if got := formatBootWindow(window); got != want {
			t.Fatalf("formatBootWindow(%v) = %q, want %q", window, got, want)
		}
	}
	if got := formatBootWindow(nodeBootWindow); got != "1 minute" {
		t.Fatalf("formatBootWindow(nodeBootWindow) = %q, want the documented promise", got)
	}
}

// A cluster.json written before the origin was recorded says nothing about
// whether a talosbox.yaml backs it, so the hint must keep the `tbx up` wording
// rather than telling a long-standing up user to destroy their cluster (#267).
func TestProvisioningRecoveryHintKeepsUpWordingForUnknownOrigin(t *testing.T) {
	status := ClusterStatus{
		Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true},
		Nodes: []NodeStatus{{Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured}},
	}
	if got := provisioningRecoveryHint(status); got != "Rerun: tbx up" {
		t.Fatalf("provisioningRecoveryHint(unknown origin) = %q, want the tbx up rerun", got)
	}
}

// The imperative hint must not print a `tbx cluster create` line: status does
// not carry node sizing, so a rendered command would rebuild a materially
// different cluster after the destroy already happened (#267).
func TestProvisioningRecoveryHintNamesRecordedIntentWithoutACreateCommand(t *testing.T) {
	status := ClusterStatus{
		Name:   "qa-cil",
		Domain: "qa-cil.k8s.test",
		ProvisioningIntent: cluster.ProvisioningIntent{
			CNI: cluster.CNICilium, CSI: cluster.CSILonghorn, LB: false, Hubble: true,
		},
		BGP:          true,
		ConfigOrigin: cluster.OriginImperative,
		Nodes: []NodeStatus{
			{Name: "qa-cil-cp-1", Role: cluster.RoleControlPlane},
			{Name: "qa-cil-cp-2", Role: cluster.RoleControlPlane},
			{Name: "qa-cil-cp-3", Role: cluster.RoleControlPlane},
			{Name: "qa-cil-worker-1", Role: cluster.RoleWorker},
			{Name: "qa-cil-worker-2", Role: cluster.RoleWorker},
		},
	}
	got := provisioningRecoveryHint(status)
	if strings.Contains(got, "tbx cluster create qa-cil") {
		t.Fatalf("provisioningRecoveryHint() printed a lossy recreate command: %s", got)
	}
	for _, want := range []string{
		"tbx cluster destroy qa-cil --force",
		"--cni cilium", "--csi longhorn", "--lb=false", "--bgp", "--hubble",
		"--cp 3", "--workers 2",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("provisioningRecoveryHint() missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "--domain") {
		t.Fatalf("provisioningRecoveryHint() named the default domain: %s", got)
	}
}

// An explicit domain is part of the shape a recreate has to reproduce.
func TestProvisioningRecoveryHintNamesAnExplicitDomain(t *testing.T) {
	status := ClusterStatus{
		Name: "demo", Domain: "demo.lab.internal",
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
		ConfigOrigin:       cluster.OriginImperative,
	}
	if got := provisioningRecoveryHint(status); !strings.Contains(got, "--domain demo.lab.internal") {
		t.Fatalf("provisioningRecoveryHint() missing the explicit domain: %s", got)
	}
}

// An unsafe domain is only accepted with its opt-in, so the recorded intent
// has to name the flag or the recreate it describes would be refused (#267).
func TestProvisioningRecoveryHintNamesTheUnsafeDomainOptIn(t *testing.T) {
	status := ClusterStatus{
		Name: "demo", Domain: "demo.lab.internal", AllowUnsafeDomain: true,
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
		ConfigOrigin:       cluster.OriginImperative,
	}
	got := provisioningRecoveryHint(status)
	if !strings.Contains(got, "--domain demo.lab.internal --allow-unsafe-domain") {
		t.Fatalf("provisioningRecoveryHint() missing the unsafe-domain opt-in: %s", got)
	}
}

// A safe explicit domain needs no opt-in, and naming one would send the
// operator back with a flag the create does not want.
func TestProvisioningRecoveryHintOmitsTheOptInForSafeDomains(t *testing.T) {
	status := ClusterStatus{
		Name: "demo", Domain: "demo.lab.test",
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
		ConfigOrigin:       cluster.OriginImperative,
	}
	if got := provisioningRecoveryHint(status); strings.Contains(got, "--allow-unsafe-domain") {
		t.Fatalf("provisioningRecoveryHint() named an opt-in the cluster never took: %s", got)
	}
}

// The image is the half of the intent no error would catch on the way back:
// a recreate without --talos-version and --extensions silently builds on the
// daemon's current default with no extensions at all (#267).
func TestProvisioningRecoveryHintNamesTheImageIntent(t *testing.T) {
	status := ClusterStatus{
		Name: "qa", TalosVersion: "v1.10.5", Schematic: "composed-id",
		BaseSchematic: "base-id", TalosExtensions: []string{"gvisor", "iscsi-tools"},
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true},
		ConfigOrigin:       cluster.OriginImperative,
	}
	got := provisioningRecoveryHint(status)
	for _, want := range []string{"--talos-version v1.10.5", "--extensions gvisor,iscsi-tools", "--schematic base-id"} {
		if !strings.Contains(got, want) {
			t.Fatalf("provisioningRecoveryHint() missing %q: %s", want, got)
		}
	}
	// the composed id is what the extensions were folded into; replaying it
	// would compose them a second time
	if strings.Contains(got, "composed-id") {
		t.Fatalf("provisioningRecoveryHint() named the composed schematic: %s", got)
	}
}

// Without extensions the stored schematic is the one the create took, so it
// replays as-is; extensions the create never named must not be invented.
func TestProvisioningRecoveryHintNamesAnExtensionlessSchematic(t *testing.T) {
	status := ClusterStatus{
		Name: "qa", TalosVersion: DefaultTalosVersion, Schematic: "plain-id",
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
		ConfigOrigin:       cluster.OriginImperative,
	}
	got := provisioningRecoveryHint(status)
	if !strings.Contains(got, "--schematic plain-id") {
		t.Fatalf("provisioningRecoveryHint() missing the recorded schematic: %s", got)
	}
	if strings.Contains(got, "--extensions") {
		t.Fatalf("provisioningRecoveryHint() invented extensions: %s", got)
	}
}

// The hint is written to be pasted, so a cluster name carrying shell
// metacharacters must not escape into the destroy command.
func TestProvisioningRecoveryHintQuotesClusterName(t *testing.T) {
	status := ClusterStatus{
		Name:               "demo; rm -rf ~",
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true},
		ConfigOrigin:       cluster.OriginImperative,
	}
	if got := provisioningRecoveryHint(status); !strings.Contains(got, "tbx cluster destroy 'demo; rm -rf ~' --force") {
		t.Fatalf("provisioningRecoveryHint() left the cluster name unquoted: %s", got)
	}
}

func TestConvergenceHintQuotesClusterName(t *testing.T) {
	if got := convergenceHint("demo; rm -rf ~"); !strings.Contains(got, "tbx status 'demo; rm -rf ~'") {
		t.Fatalf("convergenceHint() left the cluster name unquoted: %s", got)
	}
}
