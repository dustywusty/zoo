//go:build windows

package shims

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// SetUserPath puts dir first on (on=true) or removes it from (on=false)
// the user's PATH in HKCU\Environment, then tells running programs the
// environment changed. Apps started afterwards (Orca, terminals, Explorer
// launches) see it; ones already running need a restart.
func SetUserPath(dir string, on bool) (changed bool, err error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return false, err
	}
	defer k.Close()
	cur, typ, err := k.GetStringValue("Path")
	if err != nil && err != registry.ErrNotExist {
		return false, err
	}
	next := without(cur, dir)
	if on {
		next, _ = withFirst(cur, dir)
	}
	if next == cur {
		return false, nil
	}
	if typ == registry.SZ {
		err = k.SetStringValue("Path", next)
	} else {
		err = k.SetExpandStringValue("Path", next) // the usual type: keeps %VARS% working
	}
	if err != nil {
		return false, err
	}
	broadcastEnvChange()
	return true, nil
}

func broadcastEnvChange() {
	const hwndBroadcast, wmSettingChange, smtoAbortIfHung = 0xffff, 0x001A, 0x0002
	env, _ := syscall.UTF16PtrFromString("Environment")
	var result uintptr
	windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW").Call(
		hwndBroadcast, wmSettingChange, 0, uintptr(unsafe.Pointer(env)), smtoAbortIfHung, 5000, uintptr(unsafe.Pointer(&result)))
}
