//go:build windows

package main

import (
	"fmt"
	"path/filepath"
	"syscall"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

type windowsBuffer struct {
	ptr unsafe.Pointer
	len uintptr
}

type windowsHostAPI struct {
	abiVersion uint32
	hostCtx    uintptr
	call       uintptr
	freeBuffer uintptr
}

type windowsPluginAPI struct {
	abiVersion uint32
	call       uintptr
	freeBuffer uintptr
	shutdown   uintptr
}

// Package-level rather than on a goroutine stack, which can move while a plugin
// call re-enters Go through a host callback.
var (
	hostAPI = windowsHostAPI{
		call: syscall.NewCallback(func(_, _, _, _ uintptr, response *windowsBuffer) uintptr {
			onHostCall()
			if response != nil {
				*response = windowsBuffer{}
			}
			return 1
		}),
		freeBuffer: syscall.NewCallback(func(_ unsafe.Pointer, _ uintptr) uintptr { return 0 }),
	}
	pluginAPI windowsPluginAPI
	response  windowsBuffer
)

type windowsLibrary struct{}

func open(path string) (library, error) {
	// LoadLibrary wants backslashes and, given a bare name, searches the DLL path.
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dll, err := syscall.LoadDLL(abs)
	if err != nil {
		return nil, err
	}
	proc, err := dll.FindProc("cliproxy_plugin_init")
	if err != nil {
		return nil, err
	}
	hostAPI.abiVersion = pluginabi.ABIVersion
	if rc, _, _ := proc.Call(uintptr(unsafe.Pointer(&hostAPI)), uintptr(unsafe.Pointer(&pluginAPI))); rc != 0 {
		return nil, fmt.Errorf("cliproxy_plugin_init returned %d", rc)
	}
	if err := checkTable(pluginAPI.abiVersion, pluginAPI.call != 0, pluginAPI.freeBuffer != 0, pluginAPI.shutdown != 0); err != nil {
		return nil, err
	}
	return windowsLibrary{}, nil
}

func (windowsLibrary) call(method string, request []byte) ([]byte, error) {
	cMethod, err := syscall.BytePtrFromString(method)
	if err != nil {
		return nil, err
	}
	var requestPtr *byte
	if len(request) > 0 {
		requestPtr = &request[0]
	}
	response = windowsBuffer{}
	rc, _, _ := syscall.SyscallN(pluginAPI.call,
		uintptr(unsafe.Pointer(cMethod)), uintptr(unsafe.Pointer(requestPtr)), uintptr(len(request)), uintptr(unsafe.Pointer(&response)))
	var out []byte
	if response.ptr != nil && response.len > 0 {
		out = append([]byte(nil), unsafe.Slice((*byte)(response.ptr), response.len)...)
	}
	if response.ptr != nil {
		syscall.SyscallN(pluginAPI.freeBuffer, uintptr(response.ptr), response.len)
	}
	if rc != 0 {
		return nil, fmt.Errorf("plugin call %s returned %d: %s", method, rc, out)
	}
	return out, nil
}

// The host never unloads a Go DLL on Windows, so neither does this.
func (windowsLibrary) shutdown() error {
	syscall.SyscallN(pluginAPI.shutdown)
	return nil
}
