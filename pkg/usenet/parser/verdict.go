package parser

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/manifest"
)

type ContentMissingError struct{ Err error }

func (e *ContentMissingError) Error() string { return e.Err.Error() }

func (e *ContentMissingError) Unwrap() error { return e.Err }

type SegmentHolesError struct {
	File                    string
	Present, Span, Repeated int
}

func (e *SegmentHolesError) Error() string {
	return fmt.Sprintf("file %q segment numbers are not contiguous: %d of %d numbers present, %d repeated",
		e.File, e.Present, e.Span, e.Repeated)
}

type proofWithheldError struct{ err error }

func (e *proofWithheldError) Error() string { return e.err.Error() }

func (e *proofWithheldError) Unwrap() error { return e.err }

type Verdict uint8

const (
	VerdictNone Verdict = iota
	VerdictArticlesMissing
	VerdictSegmentsMissing
)

type verdictNode interface {
	verdict() Verdict
}

func (*ContentMissingError) verdict() Verdict { return VerdictArticlesMissing }

func (*SegmentHolesError) verdict() Verdict { return VerdictSegmentsMissing }

func (*proofWithheldError) verdict() Verdict { return VerdictNone }

func VerdictOf(err error) Verdict {
	if err == nil {
		return VerdictNone
	}
	if node, ok := err.(verdictNode); ok {
		return node.verdict()
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return joinedVerdict(joined.Unwrap())
	}
	if wrapper, ok := err.(interface{ Unwrap() error }); ok {
		return VerdictOf(wrapper.Unwrap())
	}
	return VerdictNone
}

func joinedVerdict(children []error) Verdict {
	verdict := VerdictNone
	for i, child := range children {
		got := VerdictOf(child)
		if got == VerdictNone || (i > 0 && got != verdict) {
			return VerdictNone
		}
		verdict = got
	}
	return verdict
}

func (p *NZBParser) proofNameAllowed(group *FileGroup, nzbName string) bool {
	if group.Type != storage.NZBFileTypeMedia {
		return true
	}
	ext := determineExtension(group)
	if ext == "" {
		ext = filepath.Ext(group.ActualFilename)
	}
	names := []string{group.ActualFilename, group.BaseName + ext, nzbName + ext}
	for _, file := range group.Files {
		names = append(names, file.Filename)
	}
	for _, name := range names {
		if name == "" {
			continue
		}
		if p.fileAllowed(name, 0) != nil {
			return false
		}
	}
	return true
}

func (p *NZBParser) refusesNamedFile(name string) bool {
	ext := filepath.Ext(strings.ToLower(name))
	return strings.Trim(strings.TrimPrefix(ext, "."), "0123456789") != "" && p.fileAllowed(name, 0) != nil
}

func ambiguousSample(file manifest.File) bool {
	return file.Rewritten || hasRepeatedNumbers(file.Segments)
}
