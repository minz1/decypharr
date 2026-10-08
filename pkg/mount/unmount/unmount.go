// Package unmount detaches FUSE mounts that decypharr no longer serves,
// such as a mount left behind by a crashed rclone or a failed start.
package unmount

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// Timeout bounds one Unmount call: every syscall and helper attempt together.
const Timeout = 10 * time.Second

// wrapperDir holds NixOS's setuid wrappers, which a systemd service's PATH
// does not include.
const wrapperDir = "/run/wrappers/bin"

var errNoMountPath = errors.New("mount path is not set")

// Unmounter detaches mounts. The unmount syscall is tried first; it needs
// privilege (root or CAP_SYS_ADMIN). An unprivileged service falls back to
// the setuid fusermount helpers, resolved to absolute paths once, at New.
type Unmounter struct {
	helpers []string
	log     zerolog.Logger
}

// New resolves the FUSE helpers installed on this host, in the order they
// are tried: fusermount, then fusermount3.
func New(log zerolog.Logger) *Unmounter {
	u := &Unmounter{log: log}
	for _, name := range []string{"fusermount", "fusermount3"} {
		if path, ok := lookHelper(name); ok {
			u.helpers = append(u.helpers, path)
		}
	}
	return u
}

// lookHelper finds name in PATH or the NixOS wrapper directory and returns
// its absolute path.
func lookHelper(name string) (string, bool) {
	for _, candidate := range []string{name, filepath.Join(wrapperDir, name)} {
		path, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		if abs, absErr := filepath.Abs(path); absErr == nil {
			return abs, true
		}
	}
	return "", false
}

// Unmount detaches the mount at mountPath, lazily if it is busy: the unmount
// syscall first, then each FUSE helper. It returns nil as soon as one
// attempt succeeds. A nil Unmounter only tries the syscall.
func (u *Unmounter) Unmount(ctx context.Context, mountPath string) error {
	path, err := cleanMountPath(mountPath)
	if err != nil {
		return err
	}
	if u == nil {
		return unmountSyscall(path)
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	syscallErr := unmountSyscall(path)
	if syscallErr == nil {
		return nil
	}
	errs := []error{syscallErr}
	for _, helper := range u.helpers {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		helperErr := runHelper(ctx, helper, path)
		if helperErr == nil {
			u.log.Info().Str("helper", helper).Str("path", path).Msg("Unmounted with the FUSE helper")
			return nil
		}
		errs = append(errs, helperErr)
	}
	return fmt.Errorf("unmount %s: %w", path, errors.Join(errs...))
}

// cleanMountPath accepts only an absolute path other than the root, so a
// misconfigured empty or relative path never reaches the helper.
func cleanMountPath(mountPath string) (string, error) {
	if strings.TrimSpace(mountPath) == "" {
		return "", errNoMountPath
	}
	path := filepath.Clean(mountPath)
	if !filepath.IsAbs(path) || path == string(filepath.Separator) || strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("refusing to unmount %q: want an absolute mount path", mountPath)
	}
	return path, nil
}

// runHelper runs `<helper> -u -z -- <path>`: a lazy unmount through the
// setuid FUSE helper. helper is an absolute path resolved by New and path
// was validated by cleanMountPath; "--" ends option parsing.
func runHelper(ctx context.Context, helper, path string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, helper, "-u", "-z", "--", path)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s: %w: %s", filepath.Base(helper), err, msg)
		}
		return fmt.Errorf("%s: %w", filepath.Base(helper), err)
	}
	return nil
}
