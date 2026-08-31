package main

import "time"

type realHelperStateTicker struct {
	ticker *time.Ticker
}

func newHelperStateTicker(interval time.Duration) helperStateTicker {
	return &realHelperStateTicker{ticker: time.NewTicker(interval)}
}

func (t *realHelperStateTicker) C() <-chan time.Time { return t.ticker.C }
func (t *realHelperStateTicker) Stop()               { t.ticker.Stop() }
