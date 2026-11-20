package parser

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sirrobot01/decypharr/internal/config"
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
	verdict() (Verdict, error)
}

func (e *ContentMissingError) verdict() (Verdict, error) { return VerdictArticlesMissing, e }

func (e *SegmentHolesError) verdict() (Verdict, error) { return VerdictSegmentsMissing, e }

func (e *proofWithheldError) verdict() (Verdict, error) { return VerdictNone, e }

func VerdictOf(err error) Verdict {
	if node, ok := err.(verdictNode); ok {
		if verdict, self := node.verdict(); any(self) == any(err) {
			return verdict
		}
		return VerdictNone
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
	names := []string{group.ActualFilename, group.BaseName + ext, soleOutputName(nzbName, ext)}
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

func (p *NZBParser) refusesNamedFile(name, nzbName string) bool {
	if p.refusesSample(name) || p.refusesSample(nzbName) || p.refusesSample(nzbName+".mkv") {
		return true
	}
	stem := name
	for numericExtension(filepath.Ext(stem)) {
		stem = strings.TrimSuffix(stem, filepath.Ext(stem))
	}
	if kind := p.detectFileType(stem); kind == storage.NZBFileTypeRar || kind == storage.NZBFileTypeZip {
		return false
	}
	return filepath.Ext(stem) != "" && p.fileAllowed(stem, 0) != nil
}

func soleOutputName(nzbName, fileExt string) string {
	if strings.EqualFold(filepath.Ext(nzbName), fileExt) {
		return nzbName
	}
	return nzbName + fileExt
}

func numericExtension(ext string) bool {
	return len(ext) > 1 && strings.Trim(ext[1:], "0123456789") == ""
}

func ambiguousSample(file manifest.File) bool {
	return file.Rewritten || hasRepeatedNumbers(file.Segments)
}

func (p *NZBParser) refusesSample(name string) bool {
	return errors.Is(p.fileAllowed(name, 0), config.ErrFileIsSample)
}
