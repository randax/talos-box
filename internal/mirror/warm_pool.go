package mirror

import (
	"context"
	"sync"
)

const (
	// DefaultWarmJobs is how many blob downloads one warm request keeps in
	// flight when it does not ask for a specific number (#506).
	DefaultWarmJobs = 8
	// MaxWarmJobs bounds the blob downloads in flight across every warm
	// request the daemon serves at once, so overlapping runs cannot multiply
	// past what one registry tolerates.
	MaxWarmJobs = 16
)

// warmPool bounds a warm request's blob downloads: a per-request budget of
// `jobs` nested inside the Manager-wide cap. A slot is held for the whole
// download, so `jobs` is exactly the request's in-flight count when it runs
// alone and the shared cap is what it degrades to when it does not.
type warmPool struct {
	request  chan struct{}
	shared   chan struct{}
	inflight *keyedMutex
}

func (m *Manager) newWarmPool(jobs int) *warmPool {
	if jobs <= 0 {
		jobs = DefaultWarmJobs
	}
	if jobs > MaxWarmJobs {
		jobs = MaxWarmJobs
	}
	m.warmSlotsOnce.Do(func() { m.warmSlots = make(chan struct{}, MaxWarmJobs) })
	return &warmPool{request: make(chan struct{}, jobs), shared: m.warmSlots, inflight: &m.warmBlobLocks}
}

func (p *warmPool) jobs() int { return cap(p.request) }

func (p *warmPool) acquire(ctx context.Context) (func(), error) {
	select {
	case p.request <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case p.shared <- struct{}{}:
	case <-ctx.Done():
		<-p.request
		return nil, ctx.Err()
	}
	return func() {
		<-p.shared
		<-p.request
	}, nil
}

// warmGroup runs a ref's blob downloads and keeps the first error. Downloads
// are started in manifest order, each after its slot is held; the first error
// stops the next from starting and frees a start still waiting for a slot,
// while downloads already under way finish, so what a failing ref did fetch
// stays on disk for the rerun. Errors caused by the stop itself are dropped
// so the ref reports what actually went wrong rather than the fallout.
type warmGroup struct {
	ctx     context.Context
	stopCtx context.Context
	stop    context.CancelFunc
	wg      sync.WaitGroup
	mu      sync.Mutex
	err     error
}

func newWarmGroup(ctx context.Context) *warmGroup {
	stopCtx, stop := context.WithCancel(ctx)
	return &warmGroup{ctx: ctx, stopCtx: stopCtx, stop: stop}
}

// start holds a pool slot for fn and runs it. It reports false once a sibling
// has failed — or the caller's context is done — and nothing was started.
func (g *warmGroup) start(pool *warmPool, fn func(context.Context) error) bool {
	release, err := pool.acquire(g.stopCtx)
	if err != nil {
		return false
	}
	if g.failed() {
		release()
		return false
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		err := fn(g.ctx)
		g.mu.Lock()
		if err != nil && g.err == nil {
			g.err = err
			g.stop()
		}
		g.mu.Unlock()
		// released only after the failure is recorded, so the slot cannot
		// start a sibling the failure should have stopped
		release()
	}()
	return true
}

func (g *warmGroup) failed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.err != nil
}

// wait joins the downloads and returns the first error, or the caller's
// context error if that is what stopped the starts.
func (g *warmGroup) wait() error {
	g.wg.Wait()
	g.stop()
	if g.err != nil {
		return g.err
	}
	return g.ctx.Err()
}

// keyedMutex serializes work on one key without touching the others: two
// warms of the same tag still publish one at a time, unrelated tags no longer
// queue behind each other (#506).
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	sync.Mutex
	waiters int
}

func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = make(map[string]*keyedLock)
	}
	entry := k.locks[key]
	if entry == nil {
		entry = &keyedLock{}
		k.locks[key] = entry
	}
	entry.waiters++
	k.mu.Unlock()

	entry.Lock()
	return func() {
		entry.Unlock()
		k.mu.Lock()
		entry.waiters--
		if entry.waiters == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}
