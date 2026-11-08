package rclone

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
)

// errNoMountTable means this OS gives no mount table events; the supervisor keeps its fast probe cadence.
var errNoMountTable = errors.New("mount table events are not available on this OS")

// mountinfoFields is the number of fields before the optional ones in a /proc/self/mountinfo line; field 5 (index
// 4) is the mount point.
const mountinfoFields = 6

// countFuseAt counts the FUSE entries in a /proc/self/mountinfo table whose mount point is path. More than one means
// mounts are stacked there.
func countFuseAt(table []byte, path string) int {
	want := filepath.Clean(path)
	n := 0
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
		if !strings.HasPrefix(after[0], "fuse") {
			continue
		}
		if filepath.Clean(unescapeMountinfo(fields[4])) == want {
			n++
		}
	}
	return n
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
