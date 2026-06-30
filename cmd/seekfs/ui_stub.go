//go:build !seekfs_ui || (!production && !dev)

package main

import "errors"

func cmdUI(args []string) error {
	return errors.New(`seekfs ui requires a Wails desktop build; build with: go build -tags "seekfs_ui production" -o seekfs-ui.exe ./cmd/seekfs`)
}
