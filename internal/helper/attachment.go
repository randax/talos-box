package helper

import (
	"errors"
	"fmt"
	"log"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// AttachmentKind identifies how a hypervisor consumes a helper-provided
// network descriptor.
type AttachmentKind string

const (
	// AttachmentDatagramFD is the vmnet datagram socket used by the VZ backend.
	AttachmentDatagramFD AttachmentKind = "datagram-fd"
	// AttachmentTapFD is the tap descriptor consumed by the QEMU backend.
	AttachmentTapFD AttachmentKind = "tap-fd"
)

// ErrUnavailable reports that the helper process could not be reached.
var ErrUnavailable = errors.New("helper unavailable")

// Attachment owns the descriptor and privileged network resource returned by
// the helper. Close is idempotent.
type Attachment struct {
	Kind AttachmentKind
	File *os.File

	release func() error
	once    sync.Once
	err     error
}

func newAttachment(kind AttachmentKind, file *os.File, release func() error) *Attachment {
	return &Attachment{Kind: kind, File: file, release: release}
}

// Close releases both the descriptor and the helper-side network resource.
func (a *Attachment) Close() error {
	if a == nil {
		return nil
	}
	a.once.Do(func() {
		if a.File != nil {
			a.err = a.File.Close()
			a.File = nil
		}
		if a.release != nil {
			a.err = errors.Join(a.err, a.release())
			a.release = nil
		}
	})
	return a.err
}

// Attach creates the platform network attachment for one node. The caller
// owns the returned attachment and must close it or transfer ownership.
func Attach(cluster string, subnetIndex int, node string) (*Attachment, error) {
	client, err := Connect()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	kind, fd, attachErr := client.attach(cluster, subnetIndex, node)
	_ = client.Close()
	if attachErr != nil {
		return nil, attachErr
	}

	file := os.NewFile(uintptr(fd), cluster+"/"+node+".network")
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("wrap network descriptor %d", fd)
	}
	release := func() error {
		client, err := Connect()
		if err != nil {
			log.Printf("detach network for %s: %v (ignored)", node, err)
			return nil
		}
		defer func() { _ = client.Close() }()
		if err := client.Detach(cluster, node); err != nil {
			log.Printf("detach network for %s: %v (ignored)", node, err)
		}
		return nil
	}
	return newAttachment(kind, file, release), nil
}
