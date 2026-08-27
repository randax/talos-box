package helper

type platformAttachment struct {
	Kind AttachmentKind
	FD   int
	// subnetIndex is the subnet this attachment lives on, so DHCP keeps a
	// listener for it even before the synced state names the cluster.
	subnetIndex int

	stop func() error
}

func (a *platformAttachment) close() error {
	if a == nil || a.stop == nil {
		return nil
	}
	return a.stop()
}
