package manager

import (
	"bytes"
	"strings"
	"time"

	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/usenet/manifest"
	"github.com/sirrobot01/decypharr/pkg/usenet/parser"
)

const nzbFreshnessWindow = 6 * time.Hour

func (m *Manager) parseFailCause(err error, content []byte) string {
	var cause string
	switch parser.VerdictOf(err) {
	case parser.VerdictArticlesMissing:
		cause = grab.CauseArticlesMissing
	case parser.VerdictSegmentsMissing:
		cause = grab.CauseSegmentsMissing
	case parser.VerdictNone:
		return ""
	}
	decoded, decodeErr := manifest.Decode(bytes.NewReader(content))
	if decodeErr != nil || len(decoded.Files) == 0 {
		return ""
	}
	for _, file := range decoded.Files {
		for _, segment := range file.Segments {
			if !postableMessageID(segment.MessageID) {
				return ""
			}
		}
	}
	newest := time.Unix(int64(decoded.NewestDate), 0)
	if m.clockNow().Sub(newest).Abs() < nzbFreshnessWindow {
		return ""
	}
	return cause
}

func postableMessageID(id string) bool {
	if id == "" || strings.ContainsAny(id, "&<>") {
		return false
	}
	for i := range len(id) {
		if id[i] <= 0x20 || id[i] == 0x7f {
			return false
		}
	}
	return true
}
