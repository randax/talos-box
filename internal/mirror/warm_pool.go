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

// warmGroup runs a ref's blob downloads and keeps the first error: that error
// cancels the siblings, whose own cancellation errors are dropped so the ref
// reports what actually went wrong rather than the fallout.
type warmGroup struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
	err    error
}

func newWarmGroup(ctx context.Context) *warmGroup {
	ctx, cancel := context.WithCancel(ctx)
	return &warmGroup{ctx: ctx, cancel: cancel}
}

func (g *warmGroup) run(fn func(context.Context) error) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if err := fn(g.ctx); err != nil {
			g.once.Do(func() {
				g.err = err
				g.cancel()
			})
		}
	}()
}

func (g *warmGroup) wait() error {
	g.wg.Wait()
	g.cancel()
	return g.err
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
