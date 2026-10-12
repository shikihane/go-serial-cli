//go:build !windows

// Package picker shows file dialogs for transfers that a device starts
// from sio shell. Outside Windows there is none: the shell asks for a path
// on its input line.
package picker

import "go-serial-cli/internal/serialcmd"

// New returns nil, which makes sio shell use its inline prompt.
func New() serialcmd.FilePicker { return nil }
