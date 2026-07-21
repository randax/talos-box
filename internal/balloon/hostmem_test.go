//go:build darwin

package balloon

import "testing"

func TestHostMemReadsSaneValues(t *testing.T) {
	total, err := HostTotalMiB()
	if err != nil || total < 4096 {
		t.Fatalf("HostTotalMiB = %d, %v (want a realistic RAM size)", total, err)
	}
	free, err := HostFreeMiB()
	if err != nil || free < 0 || free > total {
		t.Fatalf("HostFreeMiB = %d, %v (want 0..total)", free, err)
	}
	t.Logf("host: total=%dMiB free=%dMiB", total, free)
}
