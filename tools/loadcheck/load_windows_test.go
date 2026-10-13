//go:build windows

package main

import (
	"syscall"
	"testing"
	"unsafe"
)

// A plugin call that re-enters Go through a host callback can move the
// caller's stack; the host's own Windows loader has a test for the same thing.
func TestCallSurvivesReentrantHostCallback(t *testing.T) {
	saved := pluginAPI
	t.Cleanup(func() { pluginAPI = saved })
	raw := []byte(`{"ok":true,"result":{}}`)
	freed := false
	pluginAPI = windowsPluginAPI{
		call: syscall.NewCallback(func(_, _ *byte, _ uintptr, out *windowsBuffer) uintptr {
			syscall.SyscallN(hostAPI.call, 0, 0, 0, 0, uintptr(unsafe.Pointer(out)))
			out.ptr, out.len = unsafe.Pointer(&raw[0]), uintptr(len(raw))
			return 0
		}),
		freeBuffer: syscall.NewCallback(func(ptr unsafe.Pointer, n uintptr) uintptr {
			freed = ptr == unsafe.Pointer(&raw[0]) && n == uintptr(len(raw))
			return 0
		}),
	}
	before := hostCalls.Load()
	got, err := windowsLibrary{}.call("probe", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(raw) || !freed || hostCalls.Load() != before+1 {
		t.Fatalf("response %q, freed %t, host callbacks %d", got, freed, hostCalls.Load()-before)
	}
}
