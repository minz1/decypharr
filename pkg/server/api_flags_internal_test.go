package server

import (
	"cmp"
	"mime"
	"net/url"
	"testing"
)

// The old fmt.Sprintf(`filename="%s"`) ended the value at an embedded quote.
func TestAttachmentDispositionRoundTrips(t *testing.T) {
	t.Parallel()
	for _, name := range []string{`Show "Pilot".mkv`, "Café S01E01.mkv", "plain.mkv"} {
		disposition, params, err := mime.ParseMediaType(attachmentDisposition(name))
		if err != nil || disposition != "attachment" || params["filename"] != name {
			t.Errorf("%q -> %q %v (%v)", name, disposition, params, err)
		}
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
