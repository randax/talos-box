package helper

// ConvergeNetworking re-asserts all helper-owned host networking state for the
// subnets the synced reservations occupy. On Linux this is the ordinary boot
// and helper-restart path; other platforms have no persistent host state to
// restore.
func ConvergeNetworking(subnets []int) error {
	return convergeNetworking(subnets)
}
