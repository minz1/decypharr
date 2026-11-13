package storage

import (
	"testing"
	"time"
)

func TestCloneSharesNothingMutable(t *testing.T) {
	t.Parallel()
	at := time.Unix(1_700_000_000, 0)
	orig := &Entry{
		InfoHash:      "h",
		Tags:          []string{"a"},
		Grab:          []byte{1},
		CompletedAt:   &at,
		ImportedAt:    &at,
		LastErrorTime: &at,
		Files:         map[string]*File{"f": {Name: "f", ByteRange: &[2]int64{1, 2}}},
		Providers: map[string]*ProviderEntry{"p": {
			Provider:     "p",
			RemovedAt:    &at,
			DownloadedAt: &at,
			Files:        map[string]*ProviderFile{"f": {Link: "l"}},
		}},
	}
	c := orig.Clone()
	later := at.Add(time.Hour)
	c.Tags[0] = "x"
	c.Grab[0] = 9
	*c.CompletedAt = later
	*c.ImportedAt = later
	*c.LastErrorTime = later
	c.Files["f"].Name = "x"
	c.Files["f"].ByteRange[0] = 9
	c.Files["g"] = &File{}
	p := c.Providers["p"]
	p.Provider = "x"
	*p.RemovedAt = later
	*p.DownloadedAt = later
	p.Files["f"].Link = "x"
	p.Files["g"] = &ProviderFile{}
	c.Providers["q"] = &ProviderEntry{}

	o := orig
	switch {
	case o.Tags[0] != "a", o.Grab[0] != 1:
		t.Fatal("slices shared")
	case !o.CompletedAt.Equal(at), !o.ImportedAt.Equal(at), !o.LastErrorTime.Equal(at):
		t.Fatal("entry times shared")
	case o.Files["f"].Name != "f", o.Files["f"].ByteRange[0] != 1, len(o.Files) != 1:
		t.Fatal("files shared")
	case o.Providers["p"].Provider != "p", len(o.Providers) != 1:
		t.Fatal("providers shared")
	case !o.Providers["p"].RemovedAt.Equal(at), !o.Providers["p"].DownloadedAt.Equal(at):
		t.Fatal("provider times shared")
	case o.Providers["p"].Files["f"].Link != "l", len(o.Providers["p"].Files) != 1:
		t.Fatal("provider files shared")
	}
}

func TestCloneOfNilIsNil(t *testing.T) {
	t.Parallel()
	var e *Entry
	if e.Clone() != nil {
		t.Fatal("Clone of nil is not nil")
	}
}
