// Package main is the native c-shared entry point for the codex-fail-autoban
// CPA plugin. It is a thin cgo shim: it implements the plugin<->host native ABI
// and delegates every decision to the cgo-free package internal/autoban, which
// is unit-tested independently.
//
// Capabilities registered:
//   - usage_plugin:    observes completed requests and, on a terminal per-account
//     auth failure (e.g. an invalidated Codex token, HTTP 401
//     authentication_error), disables or deletes the credential.
//   - scheduler:       immediately drops the banned credential from candidate
//     selection so in-flight requests stop using it.
//   - management_api:  a status page + API to inspect handled accounts and to
//     forget the in-memory ban after re-authenticating.
//
// cgo layout note: this file contains the cgo-exported functions, so its C
// preamble holds ONLY declarations (typedefs + externs). The reverse-call helper
// DEFINITIONS live in bridge_cgo.go, which exports nothing, matching the split
// CPA uses between loader_unix.go and host_callbacks_unix.go.
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

// Plugin export signatures are NON-const to match exactly what cgo generates
// from the exported Go functions (params *C.char -> char*, *C.uint8_t -> uint8_t*).
// A const-qualified declaration here would conflict with cgo's generated header.
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
*/
import "C"

import (
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

// cliproxy_plugin_init wires the host reverse-call API and registers our
// call/free/shutdown function pointers.
//
//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	if host != nil {
		// Hand the host API to bridge_cgo.go as an opaque pointer so no
		// cgo-generated C type crosses the file boundary at the Go level.
		rememberHost(unsafe.Pointer(host))
	}
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

// slogLogger adapts the standard structured logger to autoban.Logger.
type slogLogger struct{}

func (slogLogger) Info(msg string, args ...any)  { slog.Info(msg, args...) }
func (slogLogger) Warn(msg string, args ...any)  { slog.Warn(msg, args...) }
func (slogLogger) Error(msg string, args ...any) { slog.Error(msg, args...) }
