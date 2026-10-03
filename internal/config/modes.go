package config

import (
	"cmp"
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

// Parsed defaults, for a Config that was never checked (built in code).
const (
	defaultSharedDirMode  = fs.ModeSetgid | 0o770
	defaultSharedFileMode = fs.FileMode(0o660)
)

// sharedModes holds the shared modes parsed by CheckLoadable.
type sharedModes struct {
	set       bool
	dir, file fs.FileMode
}

// SharedDirModeValue is the mode for directories in the shared trees, as
// parsed by CheckLoadable (Load and every save run it).
func (c *Config) SharedDirModeValue() fs.FileMode {
	if !c.meta.modes.set {
		return defaultSharedDirMode
	}
	return c.meta.modes.dir
}

// SharedFileModeValue is the mode for files decypharr writes into the shared
// trees (downloads, .strm files and sidecars), as parsed by CheckLoadable.
func (c *Config) SharedFileModeValue() fs.FileMode {
	if !c.meta.modes.set {
		return defaultSharedFileMode
	}
	return c.meta.modes.file
}

// parseModes parses the shared modes once; an empty field means its default.
func (c *Config) parseModes() error {
	dir, err := parseMode("shared_dir_mode", cmp.Or(c.SharedDirMode, DefaultSharedDirMode))
	if err != nil {
		return err
	}
	file, err := parseMode("shared_file_mode", cmp.Or(c.SharedFileMode, DefaultSharedFileMode))
	if err != nil {
		return err
	}
	c.meta.modes = sharedModes{set: true, dir: dir, file: file.Perm()}
	return nil
}
