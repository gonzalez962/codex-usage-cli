//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// swHide is the ShowWindow command that hides a window without activating another.
const swHide = 0

// hideOwnConsoleWindow hides the console window only when this process is the
// one that allocated it.
//
// codex-usage-cli is linked as a console-subsystem binary so that running it
// from a terminal behaves like any other CLI. The side effect is that a parent
// which spawns it without CREATE_NO_WINDOW (an agent hook, a scheduler, a
// wrapper) makes Windows allocate a fresh console, which flashes on screen.
// Fixing that caller-side requires every caller to opt in; hiding it here fixes
// it once, for every caller.
//
// GetConsoleProcessList reports how many processes are attached to the console.
// Exactly one means we own a console nobody else is using, so it was allocated
// for us and hiding it loses nothing. Two or more means we inherited a terminal
// that a shell is also attached to, and hiding that would take the user's own
// window away, so we leave it alone.
func hideOwnConsoleWindow() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	user32 := syscall.NewLazyDLL("user32.dll")

	getConsoleWindow := kernel32.NewProc("GetConsoleWindow")
	getConsoleProcessList := kernel32.NewProc("GetConsoleProcessList")
	showWindow := user32.NewProc("ShowWindow")

	hwnd, _, _ := getConsoleWindow.Call()
	if hwnd == 0 {
		// No console attached at all: the parent already used CREATE_NO_WINDOW,
		// or stdio is fully redirected. Nothing to hide.
		return
	}

	var pids [4]uint32
	count, _, _ := getConsoleProcessList.Call(
		uintptr(unsafe.Pointer(&pids[0])),
		uintptr(len(pids)),
	)
	if count != 1 {
		// Shared with a shell (or the call failed, which returns 0): keep the
		// window so interactive use is unaffected.
		return
	}

	_, _, _ = showWindow.Call(hwnd, swHide)
}
