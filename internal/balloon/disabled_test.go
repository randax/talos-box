package balloon

import "testing"

func TestDisabledReadsEnv(t *testing.T) {
	for value, want := range map[string]bool{"": false, "0": false, "false": false, "1": true, "true": true, " YES ": true} {
		t.Setenv(DisableEnv, value)
		if got := Disabled(); got != want {
			t.Fatalf("Disabled() with %s=%q = %t, want %t", DisableEnv, value, got, want)
		}
	}
	for value, want := range map[string]bool{"0": true, " FALSE ": true, "no": true, "": false, "1": false, "off": false} {
		if got := RecognizedOff(value); got != want {
			t.Fatalf("RecognizedOff(%q) = %t, want %t", value, got, want)
		}
	}
}
