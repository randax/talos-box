package daemon

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/randax/talos-box/internal/shellquote"
)

// stageFunc narrates one stage of an in-flight operation to the client that
// asked for progress. The nil stageFunc is the silent default, so an operation
// can narrate without knowing whether anyone is listening — which is what lets
// the same code serve a narrating request, an older CLI, and a test.
type stageFunc func(string)

// stage narrates one line. A nil stageFunc drops it.
func (f stageFunc) stage(format string, args ...any) {
	if f == nil {
		return
	}
	f(fmt.Sprintf(format, args...))
}

// progressSink writes stage responses onto the request's own connection, ahead
// of its single final response. It is closed before that response is written,
// so a straggling goroutine can never interleave narration with the result.
type progressSink struct {
	mu      sync.Mutex
	encoder *json.Encoder
	closed  bool
}

func (p *progressSink) emit(stage string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	// A client that stopped reading must not fail the operation it asked
	// about: the write error surfaces again when the result is sent.
	_ = p.encoder.Encode(Response{Stage: stage})
}

func (p *progressSink) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
}

// convergenceHint is the closing narration of a verb that left the cluster
// booting: the operation is done, the nodes are not, and status is where that
// is watched (#273). The name is quoted: the line is meant to be pasted, and a
// cluster name may carry shell metacharacters.
func convergenceHint(clusterName string) string {
	return fmt.Sprintf("nodes are booting; watch them converge with: tbx status %s", shellquote.Quote(clusterName))
}
