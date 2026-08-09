package helper

type platformAttachment struct {
	Kind AttachmentKind
	FD   int

	stop func() error
}

func (a *platformAttachment) close() error {
	if a == nil || a.stop == nil {
		return nil
	}
	return a.stop()
}
