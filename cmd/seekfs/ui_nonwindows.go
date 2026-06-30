//go:build (production || dev) && !windows

package main

import "os/exec"

func prepareUIServiceCommand(cmd *exec.Cmd) {}
