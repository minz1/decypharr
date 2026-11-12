package rclone

import (
	"bytes"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// errNoMountTable means this OS gives no mount table events; the supervisor keeps its fast probe cadence.
var errNoMountTable = errors.New("mount table events are not available on this OS")

// mountinfoFields is the number of fields before the optional ones in a /proc/self/mountinfo line; field 5 (index
// 4) is the mount point.
const mountinfoFields = 6

// tableEntry is one FUSE line of /proc/self/mountinfo at our path: its mount ID (field 1) and its parent's (field 2).
type tableEntry struct {
	id, parent int
}

// fuseEntriesAt lists the FUSE entries in a /proc/self/mountinfo table whose mount point is path, in table order.
// More than one means mounts are stacked there.
func fuseEntriesAt(table []byte, path string) []tableEntry {
	want := filepath.Clean(path)
	var entries []tableEntry
	for line := range bytes.Lines(table) {
		pre, post, ok := strings.Cut(strings.TrimRight(string(line), "\n"), " - ")
		if !ok {
			continue
		}
		fields := strings.Fields(pre)
		after := strings.Fields(post)
		if len(fields) < mountinfoFields || len(after) == 0 {
			continue
		}
		if !strings.HasPrefix(after[0], "fuse") || filepath.Clean(unescapeMountinfo(fields[4])) != want {
			continue
		}
		id, idErr := strconv.Atoi(fields[0])
		parent, parentErr := strconv.Atoi(fields[1])
		if idErr != nil || parentErr != nil {
			continue
		}
		entries = append(entries, tableEntry{id: id, parent: parent})
	}
	return entries
}

// topEntry is the mount on top of the stack at a path: the entry no other entry there names as its parent. A mount
// made just now is that one. With several (a broken table), the last in table order wins.
func topEntry(entries []tableEntry) (int, bool) {
	for _, top := range slices.Backward(entries) {
		if !slices.ContainsFunc(entries, func(e tableEntry) bool { return e.parent == top.id }) {
			return top.id, true
		}
	}
	return 0, false
}

// hasEntry reports whether the mount with this ID is still in entries.
func hasEntry(entries []tableEntry, id int) bool {
	for _, e := range entries {
		if e.id == id {
			return true
		}
	}
	return false
}

// unescapeMountinfo undoes the kernel's octal escapes (\040 space, \011 tab, \012 newline, \134 backslash) in a
// mountinfo path. Anything else is left as it is.
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

// tablePathFor is the form of mountPath that /proc/self/mountinfo shows: absolute, with the parent's symlinks
// resolved. The mount point itself is never resolved (it may be a hung FUSE root). If the parent cannot be
// resolved, it is the cleaned absolute path, and the supervisor's self-check falls back to probing.
func tablePathFor(mountPath string) string {
	abs, err := filepath.Abs(mountPath)
	if err != nil {
		return filepath.Clean(mountPath)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return abs
	}
	return filepath.Join(parent, filepath.Base(abs))
}
