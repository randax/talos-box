package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"google.golang.org/protobuf/types/known/emptypb"
)

const rebootNoticeTTL = 15 * time.Minute

type rebootObservation struct {
	BootTime   uint64
	RebootedAt time.Time
}

// rebootLog is intentionally process-local. After a daemon restart the first
// successful sample establishes a new baseline, so a reboot that happened
// before that sample cannot be classified retroactively.
type rebootLog struct {
	mu    sync.Mutex
	nodes map[string]rebootObservation
}

func (l *rebootLog) observe(key string, bootTime uint64, now time.Time) (rebootObservation, bool) {
	observation, _, changed := l.observeTransition(key, bootTime, now)
	return observation, changed
}

func (l *rebootLog) observeTransition(key string, bootTime uint64, now time.Time) (rebootObservation, uint64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if bootTime == 0 {
		return rebootObservation{}, 0, false
	}
	if l.nodes == nil {
		l.nodes = make(map[string]rebootObservation)
	}
	previous, known := l.nodes[key]
	if !known {
		observation := rebootObservation{BootTime: bootTime}
		l.nodes[key] = observation
		return observation, 0, false
	}
	if previous.BootTime == bootTime {
		return previous, previous.BootTime, false
	}
	observation := rebootObservation{BootTime: bootTime, RebootedAt: now}
	l.nodes[key] = observation
	return observation, previous.BootTime, true
}

func (l *rebootLog) current(key string, now time.Time) (rebootObservation, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	observation, ok := l.nodes[key]
	if !ok || observation.RebootedAt.IsZero() || now.Sub(observation.RebootedAt) >= rebootNoticeTTL {
		return rebootObservation{}, false
	}
	return observation, true
}

func (l *rebootLog) forget(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.nodes, key)
}

func (l *rebootLog) forgetPrefix(prefix string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	forgetPrefix(l.nodes, prefix)
}

func (l *rebootLog) forgetAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nodes = nil
}

var probeNodeBootTime = probeNodeBootTimeLive
var readNodeSystemStat = readNodeSystemStatLive

func probeNodeBootTimeLive(clusterName, ip string) (uint64, error) {
	_, configContext, err := lookupNodeTalosContext(clusterName)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), serviceProbeTimeout)
	defer cancel()
	response, err := readNodeSystemStat(ctx, configContext, ip)
	if err != nil {
		return 0, err
	}
	for _, message := range response.GetMessages() {
		if bootTime := message.GetBootTime(); bootTime != 0 {
			return bootTime, nil
		}
	}
	return 0, nil
}

func readNodeSystemStatLive(ctx context.Context, configContext *clientconfig.Context, ip string) (*machineapi.SystemStatResponse, error) {
	connection, err := talosclient.New(ctx,
		talosclient.WithConfigContext(configContext),
		talosclient.WithDefaultGRPCDialOptions(),
		talosclient.WithEndpoints(ip),
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = connection.Close() }()
	response, err := connection.MachineClient.SystemStat(talosclient.WithNode(ctx, ip), &emptypb.Empty{})
	if err != nil {
		return nil, fmt.Errorf("read Talos system stat: %w", err)
	}
	return response, nil
}
