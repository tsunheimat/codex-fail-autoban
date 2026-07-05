// Package main is the native c-shared entry point for the codex-fail-autoban
// CPA plugin. It is a thin cgo shim: it implements the plugin<->host native ABI
// and delegates every decision to the cgo-free package internal/autoban, which
// is unit-tested independently.
//
// The cgo structure follows the canonical CLIProxyAPI example
// (examples/plugin/host-callback-auth-files/go): the host API pointer is kept in
// a C global via store_host_api, and reverse-calls into the host go through the
// static call_host_api / free_host_buffer wrappers declared in this file.
//
// Capabilities registered:
//   - usage_plugin:    observes completed requests and, on a terminal per-account
//     auth failure (e.g. an invalidated Codex token, HTTP 401
//     authentication_error), disables or deletes the credential.
//   - scheduler:       drops the banned credential from candidate selection.
//   - management_api:  a status page + API to inspect handled accounts and to
//     forget the in-memory ban after re-authenticating.
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

typedef int  (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int  cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

// The host API pointer is captured once in cliproxy_plugin_init and read by the
// reverse-call wrappers. The host owns the struct and keeps it alive for the
// plugin's lifetime. Keeping it in a C global (not a Go global) means Go code
// never races on it and Go cannot call the C function pointers directly.
static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"fmt"
	"log/slog"
	"sync"
	"unsafe"

	"codex-fail-autoban/cpasdk/pluginabi"
	"codex-fail-autoban/internal/autoban"
)

var (
	handler  *autoban.Handler
	initOnce sync.Once
)

func main() {}

func getHandler() *autoban.Handler {
	initOnce.Do(func() {
		handler = autoban.NewHandler(cHostAPI{}, autoban.OSFileOps(), slogLogger{})
	})
	return handler
}

// cliproxy_plugin_init captures the host reverse-call API and registers our
// call/free/shutdown function pointers.
//
//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	getHandler()
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, autoban.ErrorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, err := getHandler().Handle(C.GoString(method), requestBytes)
	if err != nil {
		writeResponse(response, autoban.ErrorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	_ = length
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

// cHostAPI implements autoban.HostAPI over the native reverse-call wrappers. It
// returns the raw RPC envelope bytes; internal/autoban decodes them.
type cHostAPI struct{}

func (cHostAPI) Call(method string, request []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var cReq *C.uint8_t
	if len(request) > 0 {
		cReq = (*C.uint8_t)(C.CBytes(request))
		defer C.free(unsafe.Pointer(cReq))
	}

	var resp C.cliproxy_buffer
	rc := C.call_host_api(cMethod, cReq, C.size_t(len(request)), &resp)

	var out []byte
	if resp.ptr != nil && resp.len > 0 {
		out = C.GoBytes(resp.ptr, C.int(resp.len))
	}
	if resp.ptr != nil {
		C.free_host_buffer(resp.ptr, resp.len)
	}

	// The host reports call failures by returning an {ok:false,error} envelope in
	// out (rc stays 0). A non-zero rc with no body is a hard transport failure.
	if rc != 0 && len(out) == 0 {
		return nil, fmt.Errorf("host call %s failed with code %d", method, int(rc))
	}
	return out, nil
}

// slogLogger adapts the standard structured logger to autoban.Logger.
type slogLogger struct{}

func (slogLogger) Info(msg string, args ...any)  { slog.Info(msg, args...) }
func (slogLogger) Warn(msg string, args ...any)  { slog.Warn(msg, args...) }
func (slogLogger) Error(msg string, args ...any) { slog.Error(msg, args...) }
