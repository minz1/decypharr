package manager

import "testing"

func TestSeasonParserPatterns(t *testing.T) {
	t.Parallel()
	sp := newSeasonParser()
	for name, want := range map[string]string{
		"Show.S01-S03.1080p.WEB-DL": "Show.S02.1080p.WEB-DL",
		"Show.S01-08.720p":          "Show.S02.720p",
		"Show Complete.Series":      "Show Season 02",
		"Show 1080p WEB-DL":         "Show S02 1080p WEB-DL",
		"Show S04 1080p":            "Show S04 1080p",
		"Show":                      "Show S02",
	} {
		if got := sp.replaceMultiSeasonPattern(name, 2); got != want {
			t.Errorf("replaceMultiSeasonPattern(%q) = %q, want %q", name, got, want)
		}
	}
	if got := sp.extractSeason("Show/Season 05/e01.mkv"); got != 5 {
		t.Errorf("extractSeason = %d, want 5", got)
	}
	if !sp.hasMultiSeasonIndicators("Show.Seasons 1-4") || sp.hasMultiSeasonIndicators("Show.S01E01") {
		t.Error("multi-season indicators misclassified")
	}
}
