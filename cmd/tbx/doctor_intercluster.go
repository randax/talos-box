package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/randax/talos-box/internal/daemon"
)

// interClusterProbeTimeout bounds the host leg of the probe. That path is
// host-local, so a VIP that has not answered in this long is a dead path, not a
// slow one.
const interClusterProbeTimeout = 5 * time.Second

// interClusterDialProbeTimeout bounds the sibling leg. That request is answered
// only after the lb-probe behind the source VIP has itself dialled the sibling,
// so the budget must clearly exceed the dialer's own dial timeout — otherwise a
// blackholed path trips our deadline first and the real symptom (#388) reads as
// "the probe could not be run" instead of "this direction is dead".
const interClusterDialProbeTimeout = 20 * time.Second

// interClusterProbeBodyLimit caps the dialer's answer; it is a short JSON
// document, and an unbounded read would hand a stray listener the whole
// diagnostic.
const interClusterProbeBodyLimit = 64 << 10

// vipTarget is one running cluster's live ingress VIP.
type vipTarget struct {
	cluster string
	vip     string
}

// interClusterFinding validates that the paths between running clusters carry
// traffic. Route and forwarding checks assert only that the host *could*
// forward: in #388 both passed while a pod in one cluster could not reach a
// sibling's VIP at all, which steered diagnosis away from the real fault.
//
// Two probes make the finding. The host probe is the direct path — the host's
// route into each cluster's VIP. The sibling probes go the way the dead path
// went: every cluster's ingress VIP is served by an agnhost lb-probe, whose
// /dial endpoint dials out from inside that cluster, so asking cluster A's VIP
// to dial cluster B's VIP exercises exactly the pod-to-sibling-VIP path an
// operator otherwise has to curl from inside a guest by hand.
func interClusterFinding(statuses []daemon.ClusterStatus, statusErr error, do httpDo) doctorFinding {
	finding := doctorFinding{check: "inter-cluster"}
	if do == nil {
		finding.level, finding.detail = "SKIP", "probe unavailable"
		return finding
	}
	if statusErr != nil {
		finding.level, finding.detail = "SKIP", fmt.Sprintf("cluster status unavailable: %v", statusErr)
		return finding
	}
	var targets []vipTarget
	running := 0
	for _, status := range statuses {
		if !status.Running {
			continue
		}
		running++
		if status.VIP != "" && status.VIPLive {
			targets = append(targets, vipTarget{cluster: status.Name, vip: status.VIP})
		}
	}
	if running < 2 {
		finding.level = "SKIP"
		finding.detail = fmt.Sprintf("%d cluster(s) running; inter-cluster paths need at least two", running)
		return finding
	}
	if len(targets) < 2 {
		finding.level = "SKIP"
		finding.detail = fmt.Sprintf("%d of %d running cluster(s) report a live LoadBalancer VIP; "+
			"inter-cluster paths need at least two", len(targets), running)
		return finding
	}

	var problems, advisories []string
	reachable := make(map[string]bool, len(targets))
	for _, target := range targets {
		if err := probeVIPFromHost(do, target.vip); err != nil {
			problems = append(problems, fmt.Sprintf("host → %s VIP %s: %v", target.cluster, target.vip, err))
			continue
		}
		reachable[target.cluster] = true
	}
	var pairs []siblingProbe
	for _, source := range targets {
		if !reachable[source.cluster] {
			// The dialer lives behind the VIP the host cannot reach, so its
			// silence would say nothing about the sibling paths.
			advisories = append(advisories, fmt.Sprintf(
				"%s could not be asked to dial its siblings; its own VIP is unreachable from the host", source.cluster))
			continue
		}
		for _, sibling := range targets {
			if sibling.cluster == source.cluster {
				continue
			}
			pairs = append(pairs, siblingProbe{source: source, sibling: sibling})
		}
	}
	for i, err := range runSiblingProbes(do, pairs) {
		pair := pairs[i]
		switch {
		case err == nil:
		case isInterClusterProbeUnavailable(err):
			advisories = append(advisories, fmt.Sprintf(
				"%s → %s VIP %s could not be probed: %v", pair.source.cluster, pair.sibling.cluster, pair.sibling.vip, err))
		default:
			problems = append(problems, fmt.Sprintf(
				"%s → %s VIP %s: %v", pair.source.cluster, pair.sibling.cluster, pair.sibling.vip, err))
		}
	}

	switch {
	case len(problems) != 0:
		finding.level, finding.detail = "FAIL", strings.Join(append(problems, advisories...), "; ")
	case len(advisories) != 0:
		finding.level, finding.detail = "WARN", strings.Join(advisories, "; ")
	default:
		finding.level = "PASS"
		finding.detail = fmt.Sprintf("%d cluster VIP(s) reachable from the host and from each sibling", len(targets))
	}
	return finding
}

// siblingProbe is one direction of the cluster-to-cluster matrix.
type siblingProbe struct{ source, sibling vipTarget }

// runSiblingProbes runs the matrix concurrently and returns one result per
// pair, in the order given. The sibling budget is long enough that running the
// N·(N-1) probes in series would dominate the whole doctor run.
func runSiblingProbes(do httpDo, pairs []siblingProbe) []error {
	results := make([]error, len(pairs))
	var wait sync.WaitGroup
	for i, pair := range pairs {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results[i] = probeVIPFromCluster(do, pair.source.vip, pair.sibling.vip)
		}()
	}
	wait.Wait()
	return results
}

func probeVIPFromHost(do httpDo, vip string) error {
	response, err := getWithTimeout(do, "http://"+vip+"/", interClusterProbeTimeout)
	if err != nil {
		return err
	}
	_ = response.Body.Close()
	return nil
}

// interClusterProbeUnavailable marks a probe the cluster side could not run —
// an lb-probe without the dialer, or an answer this build cannot read. It is
// advisory: it says nothing about whether the path works.
type interClusterProbeUnavailable struct{ detail string }

func (e interClusterProbeUnavailable) Error() string { return e.detail }

func isInterClusterProbeUnavailable(err error) bool {
	var unavailable interClusterProbeUnavailable
	return errors.As(err, &unavailable)
}

// probeVIPFromCluster asks the lb-probe behind sourceVIP to dial targetVIP, so
// the probe travels the cluster-to-sibling path rather than the host's.
func probeVIPFromCluster(do httpDo, sourceVIP, targetVIP string) error {
	query := url.Values{
		"host":     {targetVIP},
		"port":     {"80"},
		"request":  {"hostname"},
		"protocol": {"http"},
		"tries":    {"1"},
	}
	response, err := getWithTimeout(do, "http://"+sourceVIP+"/dial?"+query.Encode(), interClusterDialProbeTimeout)
	if err != nil {
		// The host leg already proved sourceVIP answers, so a dial request to
		// that same VIP that never comes back is the sibling path dropping
		// traffic, not an unreachable dialer. That is the #388 symptom, and it
		// is a failure, not an advisory.
		return fmt.Errorf("no answer within %s from the lb-probe behind %s, whose own VIP answers the host: %w",
			interClusterDialProbeTimeout, sourceVIP, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return interClusterProbeUnavailable{
			detail: fmt.Sprintf("lb-probe has no dial endpoint (HTTP %d)", response.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, interClusterProbeBodyLimit))
	if err != nil {
		return interClusterProbeUnavailable{detail: fmt.Sprintf("read dial answer: %v", err)}
	}
	var answer struct {
		Responses []string `json:"responses"`
		Errors    []string `json:"errors"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return interClusterProbeUnavailable{detail: fmt.Sprintf("unreadable dial answer: %v", err)}
	}
	if len(answer.Errors) != 0 {
		return fmt.Errorf("dial failed: %s", strings.Join(answer.Errors, "; "))
	}
	if len(answer.Responses) == 0 {
		return fmt.Errorf("dial returned no response")
	}
	return nil
}

func getWithTimeout(do httpDo, target string, timeout time.Duration) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	response, err := do(request)
	if err != nil {
		cancel()
		return nil, err
	}
	if response.Body == nil {
		response.Body = http.NoBody
	}
	response.Body = cancelOnClose{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

// cancelOnClose releases the probe's context with its body, so a probe never
// leaks the timer it armed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	defer c.cancel()
	return c.ReadCloser.Close()
}

// newDoctorVIPHTTPClient builds the client the VIP probes use. It is not the
// egress client: cluster VIPs are host-local addresses that no HTTP proxy can
// reach, so a client honouring HTTP_PROXY hands http://172.30.x.200/ to the
// proxy and reports a dead path on a healthy host. Stripping the proxy mirrors
// vipHTTPClient in internal/provision, which probes these same VIPs. The client
// budget covers the longer sibling leg; each request carries its own deadline.
func newDoctorVIPHTTPClient() *http.Client {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if ok {
		transport = transport.Clone()
		transport.Proxy = nil
	} else {
		// DefaultTransport was replaced with a custom RoundTripper.
		transport = &http.Transport{ForceAttemptHTTP2: true}
	}
	return &http.Client{
		Transport: transport,
		Timeout:   interClusterDialProbeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
