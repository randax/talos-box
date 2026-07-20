package vm

import "sync"

// ringBuffer retains the most recent writes up to a fixed capacity — the
// console scrollback replayed to a client on attach.
type ringBuffer struct {
	mu    sync.Mutex
	data  []byte
	start int
	size  int
}

func newRingBuffer(capacity int) *ringBuffer {
	return &ringBuffer{data: make([]byte, capacity)}
}

func (r *ringBuffer) Write(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	capacity := len(r.data)
	if len(p) >= capacity {
		copy(r.data, p[len(p)-capacity:])
		r.start, r.size = 0, capacity
		return
	}
	for _, b := range p {
		index := (r.start + r.size) % capacity
		r.data[index] = b
		if r.size < capacity {
			r.size++
		} else {
			r.start = (r.start + 1) % capacity
		}
	}
}

func (r *ringBuffer) Snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]byte, r.size)
	for i := 0; i < r.size; i++ {
		out[i] = r.data[(r.start+i)%len(r.data)]
	}
	return out
}
