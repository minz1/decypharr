package cgofuse

import (
	"testing"

	"github.com/winfsp/cgofuse/fuse"
)

// The read-only check reads the host's FUSE flags. On Windows those differ
// from package os's values (O_APPEND is 0x8 there, [os.O_APPEND] 0x400), so a
// check written with os constants let append and create opens through.
func TestOpenForWrite(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		flags int
		want  bool
	}{
		"read only":  {fuse.O_RDONLY, false},
		"write only": {fuse.O_WRONLY, true},
		"read write": {fuse.O_RDWR, true},
		"append":     {fuse.O_RDONLY | fuse.O_APPEND, true},
		"create":     {fuse.O_RDONLY | fuse.O_CREAT, true},
		"truncate":   {fuse.O_RDONLY | fuse.O_TRUNC, true},
	}
	for name, tt := range tests {
		if got := openForWrite(tt.flags); got != tt.want {
			t.Errorf("%s (%#x): openForWrite = %v, want %v", name, tt.flags, got, tt.want)
		}
	}
}
