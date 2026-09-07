//go:build !windows

package main

// hideOwnConsoleWindow is a no-op outside Windows. Unix-like systems never
// allocate a console window for a spawned process, so there is nothing to hide
// and the CLI behaves identically whether it is run from a terminal or from an
// agent hook. See console_windows.go for the Windows behaviour.
func hideOwnConsoleWindow() {}
