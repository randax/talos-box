package helper

import (
	"fmt"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/domain"
)

// DNSRegistration describes the host resolver routing configured for a
// helper-bound cluster DNS listener. A false Registered value is deliberately
// non-fatal: guest DNS still works and doctor can present ManualStep.
type DNSRegistration struct {
	Registered bool   `json:"registered"`
	Domain     string `json:"domain"`
	Detail     string `json:"detail,omitempty"`
	ManualStep string `json:"manualStep,omitempty"`
}

type dnsCommandRunner func(string, ...string) error

var (
	bindDNS       = platformBindDNS
	registerDNS   = platformRegisterDNS
	unregisterDNS = platformUnregisterDNS
)

// applyResolvedRegistration routes clusterDomain (the cluster's effective
// domain) to the cluster bridge via systemd-resolved.
func applyResolvedRegistration(clusterDomain string, subnetIndex int, run dnsCommandRunner) DNSRegistration {
	bridge := bridgeNameForSubnet(subnetIndex)
	domain := "~" + clusterDomain
	manualStep := fmt.Sprintf(
		"sudo resolvectl dns %s %s && sudo resolvectl domain %s %q",
		bridge, cluster.Gateway(subnetIndex), bridge, domain,
	)
	for _, command := range [][]string{
		{"resolvectl", "dns", bridge, cluster.Gateway(subnetIndex)},
		{"resolvectl", "domain", bridge, domain},
	} {
		if err := run(command[0], command[1:]...); err != nil {
			return DNSRegistration{
				Domain:     domain,
				Detail:     fmt.Sprintf("systemd-resolved registration unavailable: %v", err),
				ManualStep: manualStep,
			}
		}
	}
	return DNSRegistration{Registered: true, Domain: domain, ManualStep: manualStep}
}

// decodeDNSIdentity returns the cluster's effective domain and subnet index.
// An absent domain means the default, <cluster>.k8s.test.
func decodeDNSIdentity(raw []byte) (string, int, error) {
	var args struct {
		Cluster     string `json:"cluster"`
		Domain      string `json:"domain"`
		SubnetIndex *int   `json:"subnetIndex"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return "", 0, err
	}
	if args.Cluster == "" {
		return "", 0, fmt.Errorf("cluster is required")
	}
	if args.SubnetIndex == nil {
		return "", 0, fmt.Errorf("subnetIndex is required")
	}
	if *args.SubnetIndex < 0 || *args.SubnetIndex > cluster.MaxSubnetIndex {
		return "", 0, fmt.Errorf("subnet index %d is outside 0..%d", *args.SubnetIndex, cluster.MaxSubnetIndex)
	}
	name := args.Domain
	if name == "" {
		name = args.Cluster + "." + cluster.DefaultDomainSuffix
	}
	// The helper runs as root and hands this string to resolvectl; refuse
	// anything but a canonical validated domain — derived defaults included —
	// so a buggy or hostile client cannot register e.g. "~." and hijack all
	// host DNS.
	canonical, err := domain.Validate(name, true)
	if err != nil {
		return "", 0, fmt.Errorf("refuse DNS registration: %w", err)
	}
	if canonical != name {
		return "", 0, fmt.Errorf("refuse DNS registration: domain %q is not canonical", name)
	}
	return name, *args.SubnetIndex, nil
}

func decodeDNSSubnet(raw []byte) (int, error) {
	var args struct {
		SubnetIndex *int `json:"subnetIndex"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return 0, err
	}
	if args.SubnetIndex == nil || *args.SubnetIndex < 0 || *args.SubnetIndex > cluster.MaxSubnetIndex {
		return 0, fmt.Errorf("subnetIndex must be between 0 and %d", cluster.MaxSubnetIndex)
	}
	return *args.SubnetIndex, nil
}

func (s *Server) dnsListenerReply(raw []byte) serverReply {
	clusterDomain, subnetIndex, err := decodeDNSIdentity(raw)
	if err != nil {
		return serverReply{response: failure(err), fd: -1}
	}
	file, err := bindDNS(s.desiredSubnetIndexes(), subnetIndex)
	if err != nil {
		return serverReply{response: failure(err), fd: -1}
	}
	return serverReply{
		response: success(registerDNS(clusterDomain, subnetIndex)),
		fd:       int(file.Fd()),
		finalize: func() { _ = file.Close() },
	}
}
