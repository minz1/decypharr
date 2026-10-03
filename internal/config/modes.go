package config

import (
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

// Defaults for the shared trees (download folder, symlink and STRM exports,
// mount points), which the *arr apps and media servers read and write
// through a shared group: group access, no access for others, and setgid
// directories so new files inherit the group. The process umask still
// applies on creation.
const (
	DefaultSharedDirMode  = "2770"
	DefaultSharedFileMode = "0660"
)

// maxModeBits is the largest mode the octal fields accept: permission bits
// plus setuid, setgid and sticky.
const maxModeBits = 0o7777

// parseMode reads an octal mode such as "2770" or "0o660".
func parseMode(field, value string) (fs.FileMode, error) {
	text := strings.TrimPrefix(strings.TrimSpace(value), "0o")
	bits, err := strconv.ParseUint(text, 8, 16)
	if err != nil || bits > maxModeBits {
		return 0, fmt.Errorf("%s %q is not an octal file mode", field, value)
	}
	mode := fs.FileMode(bits) & fs.ModePerm
	if bits&0o4000 != 0 {
		mode |= fs.ModeSetuid
	}
	if bits&0o2000 != 0 {
		mode |= fs.ModeSetgid
	}
	if bits&0o1000 != 0 {
		mode |= fs.ModeSticky
	}
	return mode, nil
}

// SharedDirModeValue is the mode for directories in the shared trees. An
// unset or invalid value means DefaultSharedDirMode; Load rejects invalid
// values.
func (c *Config) SharedDirModeValue() fs.FileMode {
	if mode, err := parseMode("shared_dir_mode", c.SharedDirMode); err == nil {
		return mode
	}
	mode, _ := parseMode("shared_dir_mode", DefaultSharedDirMode)
	return mode
}

// SharedFileModeValue is the mode for files decypharr writes into the shared
// trees (.strm files and sidecars). An unset or invalid value means
// DefaultSharedFileMode.
func (c *Config) SharedFileModeValue() fs.FileMode {
	if mode, err := parseMode("shared_file_mode", c.SharedFileMode); err == nil {
		return mode.Perm()
	}
	mode, _ := parseMode("shared_file_mode", DefaultSharedFileMode)
	return mode.Perm()
}

// validateModes rejects unparsable shared modes.
func (c *Config) validateModes() error {
	if _, err := parseMode("shared_dir_mode", c.SharedDirMode); err != nil {
		return err
	}
	if _, err := parseMode("shared_file_mode", c.SharedFileMode); err != nil {
		return err
	}
	return nil
}
