package helper

import "sync/atomic"

type failureLogLimiter struct {
	count uint64
}

func (l *failureLogLimiter) ShouldLog() bool {
	count := atomic.AddUint64(&l.count, 1)
	return count == 1 || count&(count-1) == 0
}
