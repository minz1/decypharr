package parser

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/nntp"
)

type blockingSource struct{ headerSource }

func (blockingSource) Header(ctx context.Context, _ string, _ int) (*nntp.YencMetadata, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A parse that runs out of time must say so; it used to report "no valid file groups found".
func TestParseReportsDeadlineInsteadOfNoGroups(t *testing.T) {
	t.Parallel()
	nzb := `<?xml version="1.0"?><nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` +
		`<file poster="p" date="1" subject="[1/1] - &quot;a8f3k2q9z7&quot; yEnc (1/1)">` +
		`<groups><group>alt.test</group></groups>` +
		`<segments><segment bytes="768000" number="1">seg-1@x</segment></segments></file></nzb>`
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, _, err := NewParserWithSource(blockingSource{}, 2, zerolog.Nop()).Parse(ctx, "release.nzb", []byte(nzb))
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
	_, _, err := NewParserWithSource(blockingSource{}, 2, zerolog.Nop()).Parse(ctx, "release.nzb", []byte(nzb))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Parse() error = %v, want a deadline error", err)
	}
}
