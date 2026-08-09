package helper

import "testing"

func TestFailureLogLimiterLogsFirstAndPowersOfTwo(t *testing.T) {
	t.Parallel()

	var limiter failureLogLimiter
	var logged []int
	for i := 1; i <= 10; i++ {
		if limiter.ShouldLog() {
			logged = append(logged, i)
		}
	}

	want := []int{1, 2, 4, 8}
	if len(logged) != len(want) {
		t.Fatalf("logged counts = %v, want %v", logged, want)
	}
	for i := range want {
		if logged[i] != want[i] {
			t.Fatalf("logged counts = %v, want %v", logged, want)
		}
	}
}
