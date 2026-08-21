package main

import "strings"

// coldBootMarker is the phrase every per-node cold-boot warning carries, in
// both the missing-save and the failed-restore wording.
const coldBootMarker = "cold-booting instead"

// coldBootedNodeCount counts the nodes a resume cold-booted. The summary line
// must not be able to contradict the warnings below it, so it carries the
// count (#411). Warnings is the per-warning list; joined is the pre-#291
// single-string form a skewed daemon may still answer with.
func coldBootedNodeCount(warnings []string, joined string) int {
	if len(warnings) == 0 {
		return strings.Count(joined, coldBootMarker)
	}
	count := 0
	for _, warning := range warnings {
		if strings.Contains(warning, coldBootMarker) {
			count++
		}
	}
	return count
}
