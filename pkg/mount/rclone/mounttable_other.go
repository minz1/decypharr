//go:build !linux

package rclone

import "context"

// watchMountTable has no mount table events to watch outside Linux.
func watchMountTable(context.Context, func()) error { return errNoMountTable }

// readMountTable has no mount table to read outside Linux.
func readMountTable(string) (int, error) { return 0, errNoMountTable }
