package daemon

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hostport"
)

// stubBGPPortListeners pins the host inventory so no test shells out.
func stubBGPPortListeners(t *testing.T, listeners []hostport.Listener, err error) {
	t.Helper()
	original := bgpPortListeners
	bgpPortListeners = func() ([]hostport.Listener, error) { return listeners, err }
	t.Cleanup(func() { bgpPortListeners = original })
}

func bgpEnabledCluster(t *testing.T, name string) cluster.Cluster {
	t.Helper()
	item, err := cluster.New(name, 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	return item
}

// `bgp enable` reported plain success next to an orphaned `nc -l 179` holding
// every address on the port, and the operator paid for that silence in
// diagnosis time (#359).
func TestBGPEnableReportsAWildcardSquatterOnThePort(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item := bgpEnabledCluster(t, "qa-a")
	stubBGPPortListeners(t, []hostport.Listener{
		{Address: "*", Line: "tcp4       0      0  *.179                  *.*                    LISTEN"},
	}, nil)
	original := connectBGPHelper
	connectBGPHelper = func() (bgpHelperClient, error) { return &legacyBGPClient{}, nil }
	t.Cleanup(func() { connectBGPHelper = original })
	raw, err := json.Marshal(nameArgs{Name: item.Name})
	if err != nil {
		t.Fatal(err)
	}

	result, err := (&Server{}).setBGP(raw, true)
	if err != nil {
		t.Fatalf("setBGP() error = %v, want the enable to succeed with an advisory", err)
	}

	if !result.BGP {
		t.Fatalf("result = %+v, want BGP enabled", result)
	}
	if !strings.Contains(result.Warning, "*.179") {
		t.Fatalf("warning = %q, want the squatting listener quoted", result.Warning)
	}
	if !strings.Contains(result.Warning, cluster.Gateway(item.SubnetIndex)) {
		t.Fatalf("warning = %q, want the speaker's own bind named", result.Warning)
	}
	if !strings.Contains(result.Warning, "sudo lsof -nP -iTCP:179 -sTCP:LISTEN") {
		t.Fatalf("warning = %q, want the way to identify the owner", result.Warning)
	}
}

// The speaker's own gateway bind — and another cluster's — is not a squatter,
// and an inventory the host would not produce says nothing about the port.
func TestBGPPortSquatterWarningStaysSilentWithoutAWildcardListener(t *testing.T) {
	item := cluster.Cluster{Name: "qa-a"}
	for _, test := range []struct {
		name      string
		listeners []hostport.Listener
		err       error
	}{
		{name: "no listener at all"},
		{name: "the speaker's own binds", listeners: []hostport.Listener{
			{Address: cluster.Gateway(0), Line: "tcp4 0 0 " + cluster.Gateway(0) + ".179 *.* LISTEN"},
			{Address: cluster.Gateway(1), Line: "tcp4 0 0 " + cluster.Gateway(1) + ".179 *.* LISTEN"},
		}},
		{name: "an inventory the host refused", err: errors.New("netstat: command not found")},
	} {
		t.Run(test.name, func(t *testing.T) {
			stubBGPPortListeners(t, test.listeners, test.err)
			if warning := bgpPortSquatterWarning(item); warning != "" {
				t.Fatalf("warning = %q, want none", warning)
			}
		})
	}
}

// Disabling withdraws the speaker; a listener left on the port afterwards is
// nothing the verb can act on, so it must not print an advisory about it.
func TestBGPDisableDoesNotReportThePortInventory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item := bgpEnabledCluster(t, "qa-a")
	stubBGPPortListeners(t, []hostport.Listener{{Address: "*", Line: "tcp4 0 0 *.179 *.* LISTEN"}}, nil)
	original := connectBGPHelper
	connectBGPHelper = func() (bgpHelperClient, error) { return &fakeBGPClient{}, nil }
	t.Cleanup(func() { connectBGPHelper = original })
	raw, err := json.Marshal(nameArgs{Name: item.Name})
	if err != nil {
		t.Fatal(err)
	}

	result, err := (&Server{}).setBGP(raw, false)
	if err != nil {
		t.Fatal(err)
	}

	if result.Warning != "" {
		t.Fatalf("warning = %q, want none on disable", result.Warning)
	}
}
