package parser

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/nntp"
)

const obfuscatedNZB = `<?xml version="1.0"?><nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` +
	`<file poster="p" date="1" subject="[1/1] - &quot;a8f3k2q9z7&quot; yEnc (1/1)">` +
	`<groups><group>alt.test</group></groups>` +
	`<segments><segment bytes="768000" number="1">seg-1@x</segment></segments></file></nzb>`

type blockingSource struct{ headerSource }

func (blockingSource) Header(ctx context.Context, _ string, _ int) (*nntp.YencMetadata, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A parse that runs out of time must say so; it used to report "no valid file groups found".
func TestParseReportsDeadlineInsteadOfNoGroups(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err := parseWith(ctx, blockingSource{}, obfuscatedNZB)
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "no valid file groups") {
		t.Fatalf("Parse() error = %v, want a deadline error", err)
	}
}

// Files identified before the deadline must not pass as the whole release.
func TestParseFailsWhenDeadlineCutsIdentificationShort(t *testing.T) {
	t.Parallel()
	nzb := `<?xml version="1.0"?><nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` +
		`<file poster="p" date="1" subject="[1/2] - &quot;release.mkv&quot; yEnc (1/1)">` +
		`<groups><group>alt.test</group></groups>` +
		`<segments><segment bytes="768000" number="1">seg-1@x</segment></segments></file>` +
		`<file poster="p" date="1" subject="[2/2] - &quot;a8f3k2q9z7&quot; yEnc (1/1)">` +
		`<groups><group>alt.test</group></groups>` +
		`<segments><segment bytes="768000" number="1">seg-2@x</segment></segments></file></nzb>`
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err := parseWith(ctx, blockingSource{}, nzb)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Parse() error = %v, want a deadline error", err)
	}
}

type missingSource struct{ headerSource }

func (missingSource) Header(context.Context, string, int) (*nntp.YencMetadata, error) {
	return nil, &nntp.Error{Type: nntp.ErrorTypeArticleNotFound, Code: 430, Message: "no such article"}
}

// A dead post whose files are all obfuscated must read as missing articles, not "no valid file groups".
func TestParseReportsDeadObfuscatedPostAsMissing(t *testing.T) {
	t.Parallel()
	err := parseWith(t.Context(), missingSource{}, obfuscatedNZB)
	if !errors.Is(err, customerror.ErrUsenetSegmentMissing) {
		t.Fatalf("Parse() error = %v, want ErrUsenetSegmentMissing", err)
	}
}

func TestParseKeepsNoGroupsForUnsupportedContent(t *testing.T) {
	t.Parallel()
	nzb := strings.Replace(obfuscatedNZB, "a8f3k2q9z7", "notes.nfo", 1)
	err := parseWith(t.Context(), missingSource{}, nzb)
	if err == nil || errors.Is(err, customerror.ErrUsenetSegmentMissing) ||
		!strings.Contains(err.Error(), "no valid file groups") {
		t.Fatalf("Parse() error = %v, want the no-groups error", err)
	}
}

func parseWith(ctx context.Context, source ArticleSource, nzb string) error {
	_, _, err := NewParserWithSource(source, 2, zerolog.Nop()).Parse(ctx, "release.nzb", []byte(nzb))
	return err
}

type flakySource struct{ headerSource }

func (flakySource) Header(_ context.Context, id string, _ int) (*nntp.YencMetadata, error) {
	if id == "seg-2@x" {
		return nil, errors.New("connection reset by peer")
	}
	return nil, &nntp.Error{Type: nntp.ErrorTypeArticleNotFound, Code: 430, Message: "no such article"}
}

// One file failing for a network reason means the post is not proven dead.
func TestParseDoesNotCallAPostDeadOnANetworkError(t *testing.T) {
	t.Parallel()
	nzb := strings.Replace(obfuscatedNZB, "</nzb>",
		`<file poster="p" date="1" subject="[2/2] - &quot;q7z9w2e4r6&quot; yEnc (1/1)">`+
			`<groups><group>alt.test</group></groups>`+
			`<segments><segment bytes="768000" number="1">seg-2@x</segment></segments></file></nzb>`, 1)
	err := parseWith(t.Context(), flakySource{}, nzb)
	if err == nil || errors.Is(err, customerror.ErrUsenetSegmentMissing) {
		t.Fatalf("Parse() error = %v, want the generic no-groups error", err)
	}
}
