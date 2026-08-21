package daemon

import (
	"sync"
	"time"
)

// Kubernetes readiness debounce (#418): a cluster's readiness probe failing
// once says nothing — an apiserver that blinks for a few seconds looks exactly
// like a provisioning pass that never finished. The daemon is the only place
// that can tell them apart, because only it sees the sequence of observations.

// readinessLog records, per cluster, when the Kubernetes readiness probe
// started failing. Status refreshes run outside opMu and concurrently per
// connection, so it carries its own lock.
type readinessLog struct {
	mu       sync.Mutex
	clusters map[string]unreadyRun
}

// unreadyRun is one uninterrupted run of readiness failures: when it started
// and when the daemon last actually looked. The last-observation time is what
// keeps the run honest — observations only happen when a client polls, so a
// cluster can fail, recover, and fail again entirely between two polls.
type unreadyRun struct {
	since time.Time
	last  time.Time
}

// observe records one readiness observation and reports when the current run of
// failures began, or nil when the cluster is ready.
//
// A gap longer than the escalation window starts a fresh run: the daemon was
// not watching across it, so it cannot claim the failure persisted through it,
// and inheriting the old clock would let a brand-new blip escalate straight to
// "destroy and recreate" — exactly the #418 advice the debounce exists to
// prevent.
func (l *readinessLog) observe(name string, ready bool, now time.Time) *time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ready {
		delete(l.clusters, name)
		return nil
	}
	if l.clusters == nil {
		l.clusters = make(map[string]unreadyRun)
	}
	run, seen := l.clusters[name]
	if !seen || now.Sub(run.last) > unreadyEscalationWindow || now.Before(run.since) {
		run.since = now
	}
	run.last = now
	l.clusters[name] = run
	since := run.since
	return &since
}

func (l *readinessLog) forget(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.clusters, name)
}

func (l *readinessLog) forgetAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.clusters = nil
}

// observeKubernetesReadiness stamps each status with how long its readiness
// probe has been failing, so the hints can require persistence before
// recommending anything as expensive as a destroy. Only clusters tbx
// provisions have a readiness verdict at all; the rest are left unstamped
// rather than recorded as failing forever.
func (s *Server) observeKubernetesReadiness(statuses []ClusterStatus, now time.Time) {
	for i := range statuses {
		status := &statuses[i]
		if status.CNI == "" || !status.Running {
			s.readiness.forget(status.Name)
			continue
		}
		status.KubernetesNotReadySince = s.readiness.observe(status.Name, status.KubernetesReady, now)
	}
}
