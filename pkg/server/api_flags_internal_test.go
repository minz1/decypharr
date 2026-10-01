package server

import (
	"cmp"
	"mime"
	"net/url"
	"testing"
)

// The old [fmt.Sprintf] of `filename="%s"` ended the value at an embedded quote.
func TestAttachmentDispositionRoundTrips(t *testing.T) {
	t.Parallel()
	for _, name := range []string{`Show "Pilot".mkv`, "Café S01E01.mkv", "plain.mkv"} {
		disposition, params, err := mime.ParseMediaType(attachmentDisposition(name))
		if err != nil || disposition != "attachment" || params["filename"] != name {
			t.Errorf("%q -> %q %v (%v)", name, disposition, params, err)
		}
	}
}

// Before paginate, (page-1)*limit overflowed for a huge ?page= and the
// negative offset panicked the handler.
func TestPaginateHugePage(t *testing.T) {
	t.Parallel()
	page, limit := pageParams(url.Values{"page": {"9223372036854775807"}, "limit": {"100"}}, defaultQueuePageLimit)
	items := make([]int, 250)
	got, totalPages := paginate(items, page, limit)
	if len(got) != 0 || totalPages != 3 {
		t.Fatalf("huge page = %d items of %d pages, want 0 of 3", len(got), totalPages)
	}
	if last, _ := paginate(items, 3, 100); len(last) != 50 {
		t.Fatalf("last page has %d items, want 50", len(last))
	}
	if empty, pages := paginate([]int{}, 1, 20); len(empty) != 0 || pages != 0 {
		t.Fatal("empty input must give an empty page")
	}
}

func TestQueryBoolOverridesBody(t *testing.T) {
	t.Parallel()
	q := url.Values{"a": {" Yes "}, "b": {"off"}, "c": {"maybe"}}
	if v := queryBool(q, "a"); v == nil || !*v {
		t.Fatal("yes should parse as true")
	}
	if v := queryBool(q, "b"); v == nil || *v {
		t.Fatal("off should parse as false")
	}
	if queryBool(q, "c") != nil || queryBool(q, "missing") != nil {
		t.Fatal("unrecognized or absent values must be nil")
	}
	body := new(true)
	if got := cmp.Or(queryBool(q, "b"), body); *got {
		t.Fatal("a query value must override the body")
	}
	if got := cmp.Or(queryBool(q, "missing"), body); got != body {
		t.Fatal("without a query value the body must win")
	}
	if boolOr(nil, true) != true || boolOr(queryBool(q, "b"), true) != false {
		t.Fatal("boolOr fallback broken")
	}
}
