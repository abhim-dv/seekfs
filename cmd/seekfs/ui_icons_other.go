//go:build !windows

package main

// GetFileIcon returns an empty string on non-Windows platforms; the frontend
// renders rows without icons.
func (a *UIApp) GetFileIcon(path string, isDir bool) string {
	return ""
}
