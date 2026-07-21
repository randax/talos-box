package balloon

import "testing"

// recordingVM captures SetMemoryTargetMiB calls.
type recordingVM struct {
	configured int
	target     int
}

func (r *recordingVM) ConfiguredMiB() int             { return r.configured }
func (r *recordingVM) SetMemoryTargetMiB(m int) error { r.target = m; return nil }

func TestReconcileInflatesUnderPressure(t *testing.T) {
	vms := map[string]Balloonable{
		"a": &recordingVM{configured: 4096},
		"b": &recordingVM{configured: 4096},
	}
	// host free 4096, reserve 6144 -> deficit 2048, split across two equal nodes
	Reconcile(vms, 4096, 6144, 1024)
	for name, v := range vms {
		got := v.(*recordingVM).target
		if got != 3072 {
			t.Errorf("node %s target = %d, want 3072", name, got)
		}
	}
}

func TestReconcileDeflatesWhenPressureReleases(t *testing.T) {
	v := &recordingVM{configured: 4096, target: 3000}
	vms := map[string]Balloonable{"a": v}
	// host free 8000 > reserve 6144 -> no deficit -> deflate to configured
	Reconcile(vms, 8000, 6144, 1024)
	if v.target != 4096 {
		t.Errorf("target = %d, want configured 4096 (deflated)", v.target)
	}
}

func TestReconcileRespectsFloor(t *testing.T) {
	v := &recordingVM{configured: 2048}
	vms := map[string]Balloonable{"a": v}
	// huge deficit but floor 1024 caps inflation
	Reconcile(vms, 0, 100000, 1024)
	if v.target != 1024 {
		t.Errorf("target = %d, want floor 1024", v.target)
	}
}
