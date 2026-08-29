package daemon

import (
	"context"
	"fmt"
	"strings"
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

type rebootProbe struct {
	key        string
	generation uint64
	seq        uint64
}

type rebootState struct {
	observation rebootObservation
	generation  uint64
	nextSeq     uint64
	appliedSeq  uint64
}

// rebootLog is intentionally process-local. After a daemon restart the first
// successful sample establishes a new baseline, so a reboot that happened
// before that sample cannot be classified retroactively.
type rebootLog struct {
	mu    sync.Mutex
	nodes map[string]rebootState
}

func (l *rebootLog) observe(key string, bootTime uint64, now time.Time) (rebootObservation, bool) {
	observation, _, changed := l.observeTransition(key, bootTime, now)
	return observation, changed
}

func (l *rebootLog) observeTransition(key string, bootTime uint64, now time.Time) (rebootObservation, uint64, bool) {
	observation, previous, changed, _ := l.completeObserve(l.beginObserve(key), bootTime, now)
	return observation, previous, changed
}

func (l *rebootLog) beginObserve(key string) rebootProbe {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.nodes == nil {
		l.nodes = make(map[string]rebootState)
	}
	state := l.nodes[key]
	state.nextSeq++
	l.nodes[key] = state
	return rebootProbe{key: key, generation: state.generation, seq: state.nextSeq}
}

func (l *rebootLog) completeObserve(probe rebootProbe, bootTime uint64, now time.Time) (rebootObservation, uint64, bool, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	state, ok := l.nodes[probe.key]
	if !ok || probe.generation != state.generation || probe.seq < state.appliedSeq || bootTime == 0 {
		return rebootObservation{}, 0, false, false
	}
	previous := state.observation
	state.appliedSeq = probe.seq
	if previous.BootTime == 0 {
		state.observation = rebootObservation{BootTime: bootTime}
		l.nodes[probe.key] = state
		return state.observation, 0, false, true
	}
	if previous.BootTime == bootTime {
		l.nodes[probe.key] = state
		return previous, previous.BootTime, false, true
	}
	state.observation = rebootObservation{BootTime: bootTime, RebootedAt: now}
	l.nodes[probe.key] = state
	return state.observation, previous.BootTime, true, true
}

func (l *rebootLog) current(key string, now time.Time) (rebootObservation, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	state, ok := l.nodes[key]
	observation := state.observation
	if !ok || observation.RebootedAt.IsZero() || now.Sub(observation.RebootedAt) >= rebootNoticeTTL {
		return rebootObservation{}, false
	}
	return observation, true
}

func (l *rebootLog) forget(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.nodes == nil {
		l.nodes = make(map[string]rebootState)
	}
	state := l.nodes[key]
	state.generation++
	state.observation = rebootObservation{}
	state.appliedSeq = 0
	l.nodes[key] = state
}

func (l *rebootLog) forgetPrefix(prefix string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, state := range l.nodes {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		state.generation++
		state.observation = rebootObservation{}
		state.appliedSeq = 0
		l.nodes[key] = state
	}
}

func (l *rebootLog) forgetAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, state := range l.nodes {
		state.generation++
		state.observation = rebootObservation{}
		state.appliedSeq = 0
		l.nodes[key] = state
	}
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
