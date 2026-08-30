//go:build darwin && cgo

package helper

/*
#cgo CFLAGS: -fblocks
#cgo LDFLAGS: -framework vmnet

#include <dispatch/dispatch.h>
#include <errno.h>
#include <fcntl.h>
#include <stdbool.h>
#include <stdatomic.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/socket.h>
#include <sys/uio.h>
#include <unistd.h>
#include <vmnet/vmnet.h>
#include <xpc/xpc.h>

#define TBX_VMNET_SOCKET_BUFFER_SIZE (4 * 1024 * 1024)

static int tbx_configure_socketpair_buffers(int first_fd, int second_fd);
static int tbx_vmnet_socket_buffer_size(void);

typedef struct tbx_vmnet {
	interface_ref interface;
	dispatch_queue_t queue;
	int pump_fd;
	int peer_fd;
	size_t max_packet_size;
	void *read_buffer;
	atomic_ulong drain_send_failures;
	atomic_bool stopping;
} tbx_vmnet_t;

static int tbx_configure_socketpair_buffers(int first_fd, int second_fd) {
	int buffer_size = TBX_VMNET_SOCKET_BUFFER_SIZE;
	if (setsockopt(first_fd, SOL_SOCKET, SO_SNDBUF, &buffer_size, sizeof(buffer_size)) != 0 ||
		setsockopt(first_fd, SOL_SOCKET, SO_RCVBUF, &buffer_size, sizeof(buffer_size)) != 0 ||
		setsockopt(second_fd, SOL_SOCKET, SO_SNDBUF, &buffer_size, sizeof(buffer_size)) != 0 ||
		setsockopt(second_fd, SOL_SOCKET, SO_RCVBUF, &buffer_size, sizeof(buffer_size)) != 0) {
		return -1;
	}
	return 0;
}

static int tbx_vmnet_socket_buffer_size(void) {
	return TBX_VMNET_SOCKET_BUFFER_SIZE;
}

static void tbx_dispatch_release(dispatch_object_t object) {
#if !OS_OBJECT_USE_OBJC
	dispatch_release(object);
#else
	(void)object;
#endif
}

static vmnet_return_t tbx_stop_ref(
	interface_ref interface,
	dispatch_queue_t queue,
	bool *was_scheduled
) {
	if (was_scheduled != NULL) {
		*was_scheduled = false;
	}
	__block vmnet_return_t completion_status = VMNET_FAILURE;
	dispatch_semaphore_t done = dispatch_semaphore_create(0);
	vmnet_return_t scheduled = vmnet_stop_interface(interface, queue, ^(vmnet_return_t status) {
		completion_status = status;
		dispatch_semaphore_signal(done);
	});
	if (scheduled == VMNET_SUCCESS) {
		if (was_scheduled != NULL) {
			*was_scheduled = true;
		}
		dispatch_semaphore_wait(done, DISPATCH_TIME_FOREVER);
	}
	tbx_dispatch_release(done);
	if (scheduled != VMNET_SUCCESS) {
		return scheduled;
	}
	return completion_status;
}

static void tbx_stop_and_release(interface_ref interface, dispatch_queue_t queue) {
	bool was_scheduled = false;
	(void)tbx_stop_ref(interface, queue, &was_scheduled);
	if (was_scheduled) {
		tbx_dispatch_release(queue);
	}
}

static void tbx_drain_vmnet(tbx_vmnet_t *state) {
	while (!atomic_load(&state->stopping)) {
		struct iovec iov = {
			.iov_base = state->read_buffer,
			.iov_len = state->max_packet_size,
		};
		struct vmpktdesc packet = {
			.vm_pkt_size = state->max_packet_size,
			.vm_pkt_iov = &iov,
			.vm_pkt_iovcnt = 1,
			.vm_flags = 0,
		};
		int packet_count = 1;
		vmnet_return_t status = vmnet_read(state->interface, &packet, &packet_count);
		if (status != VMNET_SUCCESS || packet_count == 0) {
			return;
		}

		ssize_t written;
		do {
			written = send(state->pump_fd, state->read_buffer, packet.vm_pkt_size, MSG_DONTWAIT);
		} while (written < 0 && errno == EINTR);
		if (written == (ssize_t)packet.vm_pkt_size) {
			continue;
		}
		unsigned long failure_count = atomic_fetch_add(&state->drain_send_failures, 1) + 1;
		if (failure_count == 1 || (failure_count & (failure_count - 1)) == 0) {
			int saved_errno = written < 0 ? errno : 0;
			fprintf(
				stderr,
				"tbx vmnet drain drop count=%lu wrote=%zd want=%zu errno=%d\n",
				failure_count,
				written,
				packet.vm_pkt_size,
				saved_errno
			);
		}
		// Dropping on socket backpressure keeps the serial vmnet event queue live.
	}
}

static void tbx_free_start_state(tbx_vmnet_t *state) {
	if (state->pump_fd >= 0) {
		close(state->pump_fd);
	}
	if (state->peer_fd >= 0) {
		close(state->peer_fd);
	}
	free(state->read_buffer);
	free(state);
}

static int tbx_vmnet_start(
	int subnet_index,
	tbx_vmnet_t **out_state,
	int *out_fd,
	int *out_errno,
	uint32_t *out_vmnet_status
) {
	*out_state = NULL;
	*out_fd = -1;
	*out_errno = 0;
	*out_vmnet_status = VMNET_SUCCESS;

	dispatch_queue_t queue = dispatch_queue_create("dev.talosbox.vmnet", DISPATCH_QUEUE_SERIAL);
	if (queue == NULL) {
		*out_errno = ENOMEM;
		return -1;
	}
	xpc_object_t description = xpc_dictionary_create(NULL, NULL, 0);
	if (description == NULL) {
		tbx_dispatch_release(queue);
		*out_errno = ENOMEM;
		return -1;
	}

	char start_address[32];
	char end_address[32];
	(void)snprintf(start_address, sizeof(start_address), "172.30.%d.1", subnet_index);
	(void)snprintf(end_address, sizeof(end_address), "172.30.%d.179", subnet_index);
	xpc_dictionary_set_uint64(description, vmnet_operation_mode_key, VMNET_SHARED_MODE);
	xpc_dictionary_set_bool(description, vmnet_allocate_mac_address_key, false);
	xpc_dictionary_set_string(description, vmnet_start_address_key, start_address);
	xpc_dictionary_set_string(description, vmnet_end_address_key, end_address);
	xpc_dictionary_set_string(description, vmnet_subnet_mask_key, "255.255.255.0");

	__block vmnet_return_t start_status = VMNET_FAILURE;
	__block uint64_t max_packet_size = 0;
	dispatch_semaphore_t started = dispatch_semaphore_create(0);
	interface_ref interface = vmnet_start_interface(
		description,
		queue,
		^(vmnet_return_t status, xpc_object_t parameters) {
			start_status = status;
			if (status == VMNET_SUCCESS && parameters != NULL) {
				max_packet_size = xpc_dictionary_get_uint64(parameters, vmnet_max_packet_size_key);
			}
			dispatch_semaphore_signal(started);
		}
	);
	if (interface == NULL) {
		tbx_dispatch_release(started);
		xpc_release(description);
		tbx_dispatch_release(queue);
		*out_vmnet_status = VMNET_FAILURE;
		return -1;
	}
	dispatch_semaphore_wait(started, DISPATCH_TIME_FOREVER);
	tbx_dispatch_release(started);
	xpc_release(description);
	if (start_status != VMNET_SUCCESS || max_packet_size == 0 || max_packet_size > SIZE_MAX) {
		tbx_stop_and_release(interface, queue);
		*out_vmnet_status = start_status == VMNET_SUCCESS ? VMNET_INVALID_ARGUMENT : start_status;
		return -1;
	}

	tbx_vmnet_t *state = calloc(1, sizeof(*state));
	if (state == NULL) {
		tbx_stop_and_release(interface, queue);
		*out_errno = ENOMEM;
		return -1;
	}
	state->interface = interface;
	state->queue = queue;
	state->pump_fd = -1;
	state->peer_fd = -1;
	state->max_packet_size = (size_t)max_packet_size;
	atomic_init(&state->stopping, false);
	atomic_init(&state->drain_send_failures, 0);

	state->read_buffer = malloc(state->max_packet_size);
	if (state->read_buffer == NULL) {
		tbx_stop_and_release(interface, queue);
		tbx_free_start_state(state);
		*out_errno = ENOMEM;
		return -1;
	}
	int sockets[2];
	if (socketpair(AF_UNIX, SOCK_DGRAM, 0, sockets) != 0) {
		int saved_errno = errno;
		tbx_stop_and_release(interface, queue);
		tbx_free_start_state(state);
		*out_errno = saved_errno;
		return -1;
	}
	state->pump_fd = sockets[0];
	state->peer_fd = sockets[1];
	if (tbx_configure_socketpair_buffers(state->pump_fd, state->peer_fd) != 0) {
		int saved_errno = errno;
		tbx_stop_and_release(interface, queue);
		tbx_free_start_state(state);
		*out_errno = saved_errno;
		return -1;
	}
	if (fcntl(state->pump_fd, F_SETFD, FD_CLOEXEC) != 0 ||
		fcntl(state->peer_fd, F_SETFD, FD_CLOEXEC) != 0) {
		int saved_errno = errno;
		tbx_stop_and_release(interface, queue);
		tbx_free_start_state(state);
		*out_errno = saved_errno;
		return -1;
	}
	int no_sigpipe = 1;
	if (setsockopt(state->pump_fd, SOL_SOCKET, SO_NOSIGPIPE, &no_sigpipe, sizeof(no_sigpipe)) != 0) {
		int saved_errno = errno;
		tbx_stop_and_release(interface, queue);
		tbx_free_start_state(state);
		*out_errno = saved_errno;
		return -1;
	}

	vmnet_return_t callback_status = vmnet_interface_set_event_callback(
		interface,
		VMNET_INTERFACE_PACKETS_AVAILABLE,
		queue,
		^(interface_event_t events, xpc_object_t event) {
			(void)event;
			if ((events & VMNET_INTERFACE_PACKETS_AVAILABLE) != 0) {
				tbx_drain_vmnet(state);
			}
		}
	);
	if (callback_status != VMNET_SUCCESS) {
		tbx_stop_and_release(interface, queue);
		tbx_free_start_state(state);
		*out_vmnet_status = callback_status;
		return -1;
	}
	*out_state = state;
	*out_fd = state->peer_fd;
	return 0;
}

static int tbx_vmnet_prepare_stop(
	tbx_vmnet_t *state,
	int *out_errno,
	uint32_t *out_callback_status
) {
	*out_errno = 0;
	*out_callback_status = VMNET_SUCCESS;
	atomic_store(&state->stopping, true);

	vmnet_return_t callback_status =
		vmnet_interface_set_event_callback(
			state->interface,
			0,
			NULL,
			NULL
		);
	dispatch_sync(state->queue, ^{});
	if (state->pump_fd >= 0 && shutdown(state->pump_fd, SHUT_RDWR) != 0 && errno != ENOTCONN) {
		int saved_errno = errno;
		(void)close(state->pump_fd);
		state->pump_fd = -1;
		fprintf(stderr, "tbx vmnet prepare stop fallback close errno=%d\n", saved_errno);
		*out_errno = saved_errno;
	}
	*out_callback_status = callback_status;
	return 0;
}

static int tbx_vmnet_finish_stop(
	tbx_vmnet_t *state,
	int *out_errno,
	uint32_t *out_vmnet_status,
	int *out_retain
) {
	*out_errno = 0;
	*out_vmnet_status = VMNET_SUCCESS;
	*out_retain = 0;
	bool stop_scheduled = false;
	vmnet_return_t stop_status = tbx_stop_ref(state->interface, state->queue, &stop_scheduled);
	if (!stop_scheduled) {
		*out_vmnet_status = stop_status;
		*out_retain = 1;
		return -1;
	}
	dispatch_sync(state->queue, ^{});

	tbx_dispatch_release(state->queue);
	tbx_free_start_state(state);
	if (stop_status != VMNET_SUCCESS) {
		*out_vmnet_status = stop_status;
		return -1;
	}
	return 0;
}

static int tbx_vmnet_write_frame(
	tbx_vmnet_t *state,
	const void *buffer,
	size_t size,
	int *out_errno,
	uint32_t *out_vmnet_status
) {
	*out_errno = 0;
	*out_vmnet_status = VMNET_SUCCESS;
	if (size == 0 || size > state->max_packet_size) {
		*out_vmnet_status = VMNET_INVALID_ARGUMENT;
		return -1;
	}
	struct iovec iov = {
		.iov_base = (void *)buffer,
		.iov_len = size,
	};
	struct vmpktdesc packet = {
		.vm_pkt_size = size,
		.vm_pkt_iov = &iov,
		.vm_pkt_iovcnt = 1,
		.vm_flags = 0,
	};
	int packet_count = 1;
	vmnet_return_t status = vmnet_write(state->interface, &packet, &packet_count);
	if (status != VMNET_SUCCESS || packet_count != 1) {
		*out_vmnet_status = status == VMNET_SUCCESS ? VMNET_FAILURE : status;
		return -1;
	}
	return 0;
}

static int tbx_vmnet_pump_fd(tbx_vmnet_t *state) {
	return state->pump_fd;
}

static size_t tbx_vmnet_max_packet_size(tbx_vmnet_t *state) {
	return state->max_packet_size;
}
*/
import "C"

import (
	"fmt"
	"log"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

type vmnetHandle struct {
	state              *C.tbx_vmnet_t
	port               *routerPort
	pumpDone           chan struct{}
	localForwardErrors failureLogLimiter
	vmnetWriteErrors   failureLogLimiter
}

var vmnetInterfaces = struct {
	sync.Mutex
	byFD map[int]*vmnetHandle
}{byFD: make(map[int]*vmnetHandle)}

var helperFrameRouter = newFrameRouter()

func configureVMNetSocketpairBuffers(firstFD, secondFD int) error {
	if result, err := C.tbx_configure_socketpair_buffers(C.int(firstFD), C.int(secondFD)); result != 0 {
		return err
	}
	return nil
}

func vmnetSocketBufferSize() int {
	return int(C.tbx_vmnet_socket_buffer_size())
}

// StartInterface starts one shared-mode vmnet interface for a cluster subnet.
func StartInterface(_ []int, subnetIndex int, _, _ string) (*platformAttachment, error) {
	if subnetIndex < 0 || subnetIndex > 255 {
		return nil, fmt.Errorf("subnet index %d is outside 0..255", subnetIndex)
	}
	var state *C.tbx_vmnet_t
	var fd C.int
	var systemError C.int
	var vmnetStatus C.uint32_t
	if C.tbx_vmnet_start(
		C.int(subnetIndex),
		&state,
		&fd,
		&systemError,
		&vmnetStatus,
	) != 0 {
		return nil, vmnetError("start", systemError, vmnetStatus)
	}

	handle := &vmnetHandle{state: state}
	handle.port = helperFrameRouter.addPort(subnetIndex, func(frame []byte) error {
		return sendPumpFrame(int(C.tbx_vmnet_pump_fd(state)), frame)
	})
	handle.pumpDone = startVMNetPump(handle)

	result := int(fd)
	vmnetInterfaces.Lock()
	vmnetInterfaces.byFD[result] = handle
	vmnetInterfaces.Unlock()
	return &platformAttachment{
		Kind: AttachmentDatagramFD,
		FD:   result,
		stop: func() error { return stopVMNetInterface(result) },
	}, nil
}

func stopVMNetInterface(fd int) error {
	vmnetInterfaces.Lock()
	handle, ok := vmnetInterfaces.byFD[fd]
	if ok {
		delete(vmnetInterfaces.byFD, fd)
	}
	vmnetInterfaces.Unlock()
	if !ok {
		return fmt.Errorf("vmnet interface for file descriptor %d is not running", fd)
	}

	helperFrameRouter.removePort(handle.port)

	var prepareSystemError C.int
	var callbackStatus C.uint32_t
	if C.tbx_vmnet_prepare_stop(handle.state, &prepareSystemError, &callbackStatus) != 0 {
		return wrapVMNetStopError(vmnetError("stop", prepareSystemError, callbackStatus), false)
	}

	<-handle.pumpDone

	var finishSystemError C.int
	var stopStatus C.uint32_t
	var finishRetain C.int
	if C.tbx_vmnet_finish_stop(handle.state, &finishSystemError, &stopStatus, &finishRetain) != 0 {
		if finishRetain != 0 {
			vmnetInterfaces.Lock()
			vmnetInterfaces.byFD[fd] = handle
			vmnetInterfaces.Unlock()
		}
		return wrapVMNetStopError(vmnetError("stop", finishSystemError, stopStatus), finishRetain != 0)
	}
	if prepareSystemError != 0 {
		return wrapVMNetStopError(vmnetError("stop", prepareSystemError, 0), false)
	}
	// macOS can report VMNET_INVALID_ARGUMENT while disabling an installed
	// callback with the documented zero-mask/nil callback form. Once the
	// subsequent vmnet_stop_interface call succeeds, the callback is inert and
	// all local state has been released, so only other callback errors matter.
	if callbackStatus != C.VMNET_SUCCESS && callbackStatus != C.VMNET_INVALID_ARGUMENT {
		return wrapVMNetStopError(vmnetError("stop", 0, callbackStatus), false)
	}
	return nil
}

func startVMNetPump(handle *vmnetHandle) chan struct{} {
	done := make(chan struct{})
	pumpFD := int(C.tbx_vmnet_pump_fd(handle.state))
	maxPacketSize := int(C.tbx_vmnet_max_packet_size(handle.state))

	go func() {
		defer close(done)

		buffer := make([]byte, maxPacketSize)
		for {
			n, _, flags, _, err := unix.Recvmsg(pumpFD, buffer, nil, 0)
			if err == unix.EINTR {
				continue
			}
			if err != nil || n <= 0 {
				return
			}
			if flags&unix.MSG_TRUNC != 0 {
				continue
			}

			frame := append([]byte(nil), buffer[:n]...)
			handled, err := helperFrameRouter.route(handle.port, frame)
			if err != nil && handle.localForwardErrors.ShouldLog() {
				log.Printf("helper vmnet local forward failed on subnet %d: %v", handle.port.subnet, err)
			}
			if handled {
				continue
			}
			if err := writeVMNetFrame(handle.state, frame); err != nil && handle.vmnetWriteErrors.ShouldLog() {
				log.Printf("helper vmnet write failed on subnet %d: %v", handle.port.subnet, err)
			}
		}
	}()

	return done
}

func sendPumpFrame(fd int, frame []byte) error {
	if len(frame) == 0 {
		return nil
	}
	_, err := unix.SendmsgN(fd, frame, nil, nil, unix.MSG_DONTWAIT)
	if err != nil {
		return fmt.Errorf("send locally forwarded frame to pump fd %d: %w", fd, err)
	}
	return nil
}

func writeVMNetFrame(state *C.tbx_vmnet_t, frame []byte) error {
	if len(frame) == 0 {
		return nil
	}
	var systemError C.int
	var vmnetStatus C.uint32_t
	if C.tbx_vmnet_write_frame(
		state,
		unsafe.Pointer(&frame[0]),
		C.size_t(len(frame)),
		&systemError,
		&vmnetStatus,
	) != 0 {
		return vmnetError("write", systemError, vmnetStatus)
	}
	// The pump goroutine owns logging for vmnet writes so errors surface once.
	return nil
}

func vmnetError(operation string, systemError C.int, vmnetStatus C.uint32_t) error {
	if systemError != 0 {
		return fmt.Errorf("%s vmnet interface: %w", operation, syscall.Errno(systemError))
	}
	return fmt.Errorf("%s vmnet interface: vmnet status %d", operation, uint32(vmnetStatus))
}
