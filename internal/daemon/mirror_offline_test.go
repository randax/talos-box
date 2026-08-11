package daemon

import (
	"encoding/json"
	"testing"

	"github.com/randax/talos-box/internal/mirror"
)

func TestMirrorOfflineDefaultsOffAndCanToggle(t *testing.T) {
	t.Parallel()

	service := &Server{mirrors: mirror.NewManager(t.TempDir())}

	got, err := service.handle(Request{Op: "mirror.offline.get", Args: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	initial, ok := got.(MirrorOfflineStatus)
	if !ok {
		t.Fatalf("get result type = %T, want MirrorOfflineStatus", got)
	}
	if initial.Enabled {
		t.Fatal("mirror offline default = on, want off")
	}

	got, err = service.handle(Request{Op: "mirror.offline.set", Args: json.RawMessage(`{"enabled":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	enabled, ok := got.(MirrorOfflineStatus)
	if !ok {
		t.Fatalf("set result type = %T, want MirrorOfflineStatus", got)
	}
	if !enabled.Enabled {
		t.Fatal("mirror offline set did not enable state")
	}
	if !service.mirrors.Offline() {
		t.Fatal("mirror manager did not observe enabled state")
	}

	got, err = service.handle(Request{Op: "mirror.offline.get", Args: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	after, ok := got.(MirrorOfflineStatus)
	if !ok {
		t.Fatalf("get-after-set result type = %T, want MirrorOfflineStatus", got)
	}
	if !after.Enabled {
		t.Fatal("mirror offline inspection did not report enabled state")
	}
}

func TestMirrorOfflineSetRejectsMissingNullAndUnknownPayloads(t *testing.T) {
	t.Parallel()

	service := &Server{mirrors: mirror.NewManager(t.TempDir())}
	for _, payload := range []string{
		`{}`,
		`{"enabled":null}`,
		`{"enabled":true,"extra":1}`,
		`{"enabled":true}{"enabled":false}`,
		`{"enabled":true,"enabled":false}`,
		`{"extra":1}`,
		`true`,
	} {
		t.Run(payload, func(t *testing.T) {
			_, err := service.handle(Request{Op: "mirror.offline.set", Args: json.RawMessage(payload)})
			if err == nil {
				t.Fatalf("mirror.offline.set accepted invalid payload %s", payload)
			}
		})
	}
}
