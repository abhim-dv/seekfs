//go:build seekfs_ui && (production || dev)

package main

import "testing"

func TestShowShellContextMenuEmptyPathsReturnsWithoutCOM(t *testing.T) {
	app := &UIApp{ctx: nil}
	shown, err := app.ShowShellContextMenu([]string{"  ", "\t", ""}, 10, 20)
	if shown {
		t.Fatal("ShowShellContextMenu(empty paths) returned shown=true")
	}
	if err != nil {
		t.Fatalf("ShowShellContextMenu(empty paths) returned error: %v", err)
	}
}

func TestShowShellContextMenuEmptyPathsListReturnsWithoutCOM(t *testing.T) {
	app := &UIApp{ctx: nil}
	shown, err := app.ShowShellContextMenu(nil, 10, 20)
	if shown {
		t.Fatal("ShowShellContextMenu(nil paths) returned shown=true")
	}
	if err != nil {
		t.Fatalf("ShowShellContextMenu(nil paths) returned error: %v", err)
	}
}
