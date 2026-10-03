//go:build !linux && !darwin && !windows

package buffer

import "os"

func prepareSparse(*os.File) error { return nil }

func punchHole(*os.File, int64, int64) (Range, error) { return Range{}, errPunchUnsupported }
