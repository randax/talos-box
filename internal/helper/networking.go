package helper

// ConvergeNetworking re-asserts all helper-owned host networking state. On
// Linux this is the ordinary boot and helper-restart path; other platforms have
// no persistent host state to restore.
func ConvergeNetworking() error {
	return convergeNetworking()
}
