//go:build !windows

package main

// foregroundWindowHandle has no native equivalent on non-Windows platforms.
// The binding passes 0 to showShellContextMenu, which immediately falls back.
func foregroundWindowHandle() uintptr {
	return 0
}

// showShellContextMenu is not supported on non-Windows platforms; the
// frontend falls back to the HTML context menu.
func showShellContextMenu(ownerHwnd uintptr, paths []string, screenX, screenY int32) (bool, error) {
	return false, nil
}

// warmUpShellContextMenus is a no-op on non-Windows platforms.
func warmUpShellContextMenus() {}

// prebuildShellContextMenu is a no-op on non-Windows platforms; the frontend
// falls back to the HTML context menu.
func prebuildShellContextMenu(paths []string) {}
