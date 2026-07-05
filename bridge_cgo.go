// bridge_cgo.go holds the plugin -> host reverse-call implementation.
//
// It deliberately contains NO cgo-exported functions, so its C preamble is
// allowed to define the static wrapper functions that let Go invoke the
// host-provided function pointers. (Go cannot call C function pointers directly.)
// This mirrors the way CPA separates loader_unix.go (wrapper definitions) from
// host_callbacks_unix.go (the exported functions).
//
// main.go hands the raw cliproxy_host_api* to rememberHost as an unsafe.Pointer,
// so no cgo-generated C type ever crosses the file boundary at the Go level.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int  (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

static int invoke_host_call(cliproxy_host_api* host, const char* method, const uint8_t* req, size_t reqlen, cliproxy_buffer* resp) {
	if (host == NULL || host->call == NULL) {
		return -1;
	}
	return host->call(host->host_ctx, method, req, reqlen, resp);
}

static void invoke_host_free(cliproxy_host_api* host, void* ptr, size_t len) {
	if (host != NULL && host->free_buffer != NULL && ptr != NULL) {
		host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"errors"
	"fmt"
	"sync/atomic"
	"unsafe"
)

// savedHost is the host API captured in cliproxy_plugin_init (main.go). The host
// owns and keeps the struct alive for the plugin's lifetime, so the pointer stays
// valid. It is published atomically so a plugin-call goroutine reading it has a
// proper happens-before edge with the init-thread write.
var savedHost atomic.Pointer[C.cliproxy_host_api]

// rememberHost stores the host API pointer. main.go passes it as an unsafe.Pointer
// so the cgo struct type never has to be shared across files.
func rememberHost(p unsafe.Pointer) {
	if p == nil {
		return
	}
	savedHost.Store((*C.cliproxy_host_api)(p))
}

// cHostAPI implements autoban.HostAPI over the native reverse-call pointers.
type cHostAPI struct{}

func (cHostAPI) Call(method string, request []byte) ([]byte, error) {
	host := savedHost.Load()
	if host == nil {
		return nil, errors.New("host reverse-call is unavailable")
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var cReq *C.uint8_t
	if len(request) > 0 {
		cReq = (*C.uint8_t)(C.CBytes(request))
		defer C.free(unsafe.Pointer(cReq))
	}

	var resp C.cliproxy_buffer
	rc := C.invoke_host_call(host, cMethod, cReq, C.size_t(len(request)), &resp)

	var out []byte
	if resp.ptr != nil && resp.len > 0 {
		out = C.GoBytes(resp.ptr, C.int(resp.len))
	}
	if resp.ptr != nil {
		C.invoke_host_free(host, resp.ptr, resp.len)
	}

	// The host reports call failures by returning an {ok:false,error} envelope in
	// out (rc stays 0). A non-zero rc with no body is a hard transport failure.
	if rc != 0 && len(out) == 0 {
		return nil, fmt.Errorf("host call %s failed with code %d", method, int(rc))
	}
	return out, nil
}
