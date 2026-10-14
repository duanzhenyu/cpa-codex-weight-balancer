//go:build linux || darwin

package main

/*
#cgo linux CFLAGS: -D_GNU_SOURCE
#cgo linux LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

typedef int (*cliproxy_plugin_init_fn)(const cliproxy_host_api*, cliproxy_plugin_api*);

extern int loadcheckHostCall(cliproxy_buffer*);

static int host_call(void* ctx, const char* method, const uint8_t* request, size_t len, cliproxy_buffer* response) {
	return loadcheckHostCall(response);
}

static void host_free(void* ptr, size_t len) {}

// The plugin keeps this pointer for its whole life.
static cliproxy_host_api host_api = {0, NULL, host_call, host_free};

static int plugin_init(void* fn, uint32_t abi_version, cliproxy_plugin_api* plugin) {
	host_api.abi_version = abi_version;
	return ((cliproxy_plugin_init_fn)fn)(&host_api, plugin);
}

static int plugin_call(cliproxy_plugin_call_fn fn, const char* method, const uint8_t* request, size_t len, cliproxy_buffer* response) {
	return fn(method, request, len, response);
}

static void plugin_free(cliproxy_plugin_free_fn fn, void* ptr, size_t len) {
	fn(ptr, len);
}

static void plugin_shutdown(cliproxy_plugin_shutdown_fn fn) {
	fn();
}

static int mapped(void* p) {
	Dl_info info;
	return dladdr(p, &info) != 0;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

//export loadcheckHostCall
func loadcheckHostCall(response *C.cliproxy_buffer) C.int {
	onHostCall()
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	return 1
}

type unixLibrary struct {
	handle unsafe.Pointer
	api    C.cliproxy_plugin_api
}

func open(path string) (library, error) {
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))
	handle := C.dlopen(cPath, C.RTLD_NOW|C.RTLD_LOCAL)
	if handle == nil {
		return nil, fmt.Errorf("dlopen %s: %s", path, C.GoString(C.dlerror()))
	}
	cSymbol := C.CString("cliproxy_plugin_init")
	defer C.free(unsafe.Pointer(cSymbol))
	entry := C.dlsym(handle, cSymbol)
	if entry == nil {
		return nil, fmt.Errorf("missing cliproxy_plugin_init: %s", C.GoString(C.dlerror()))
	}
	lib := &unixLibrary{handle: handle}
	if rc := C.plugin_init(entry, C.uint32_t(pluginabi.ABIVersion), &lib.api); rc != 0 {
		return nil, fmt.Errorf("cliproxy_plugin_init returned %d", int(rc))
	}
	if err := checkTable(uint32(lib.api.abi_version), lib.api.call != nil, lib.api.free_buffer != nil, lib.api.shutdown != nil); err != nil {
		return nil, err
	}
	return lib, nil
}

func (l *unixLibrary) call(method string, request []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var cRequest unsafe.Pointer
	if len(request) > 0 {
		cRequest = C.CBytes(request)
		defer C.free(cRequest)
	}
	var response C.cliproxy_buffer
	rc := C.plugin_call(l.api.call, cMethod, (*C.uint8_t)(cRequest), C.size_t(len(request)), &response)
	var out []byte
	if response.ptr != nil && response.len > 0 {
		out = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.plugin_free(l.api.free_buffer, response.ptr, response.len)
	}
	if rc != 0 {
		return nil, fmt.Errorf("plugin call %s returned %d: %s", method, int(rc), out)
	}
	return out, nil
}

// shutdown also unloads, as the host does when a plugin is disabled: a library
// that crashes on dlclose crashes the host with it. Its Go runtime cannot stop,
// so the image must stay mapped; on darwin an unmapped one leaves the runtime's
// threads spinning and can hang the host's exit.
func (l *unixLibrary) shutdown() error {
	C.plugin_shutdown(l.api.shutdown)
	C.dlclose(l.handle)
	if C.mapped(unsafe.Pointer(l.api.call)) == 0 {
		return errors.New("dlclose unmapped the library under its running Go runtime")
	}
	return nil
}
