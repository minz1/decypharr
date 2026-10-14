package config

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestPruneDefaults(t *testing.T) {
	t.Parallel()
	c := &Config{}
	c.applyPruneDefaults()
	p := c.Prune
	if p.Mode != PruneModeReport || p.Schedule != "1h" || p.Grace != "168h" || p.MaxPerRun != 100 {
		t.Fatalf("defaults wrong: %+v", p)
	}
	if p.Enabled || p.Roots != nil {
		t.Fatalf("enabled/roots must stay zero: %+v", p)
	}
}

func TestPruneValidate(t *testing.T) {
	t.Parallel()
	valid := func() Prune {
		p := Prune{Enabled: true, Roots: []string{"/data/library", "/data/downloads"}}
		c := &Config{Prune: p}
		c.applyPruneDefaults()
		return c.Prune
	}
	tests := []struct {
		name    string
		mutate  func(p *Prune)
		strm    bool
		wantErr bool
	}{
		{"valid", func(*Prune) {}, false, false},
		{"no roots", func(p *Prune) { p.Roots = nil }, false, true},
		{"relative root", func(p *Prune) { p.Roots = []string{"data/library"} }, false, true},
		{"bad mode", func(p *Prune) { p.Mode = "purge" }, false, true},
		{"bad grace", func(p *Prune) { p.Grace = "soon" }, false, true},
		{"bad schedule", func(p *Prune) { p.Schedule = "bogus" }, false, true},
		{"negative max", func(p *Prune) { p.MaxPerRun = -1 }, false, true},
		{"strm enabled", func(*Prune) {}, true, true},
		{"disabled no roots", func(p *Prune) { p.Enabled = false; p.Roots = nil }, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := valid()
			tt.mutate(&p)
			err := p.validate(tt.strm)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestPruneGraceDuration(t *testing.T) {
	t.Parallel()
	if got := (Prune{Grace: "168h"}).GraceDuration().Hours(); got != 168 {
		t.Fatalf("grace = %v", got)
	}
}

func TestPruneJSONRoundTrip(t *testing.T) {
	t.Parallel()
	in := []byte(`{"enabled":true,"mode":"delete","schedule":"1h","grace":"168h",` +
		`"max_per_run":50,"roots":["/data/library","/data/downloads"]}`)
	var c Config
	if err := json.Unmarshal([]byte(`{"prune":`+string(in)+`}`), &c); err != nil {
		t.Fatal(err)
	}
	c.setDefaults()
	out, err := json.Marshal(c.Prune)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, in) {
		t.Fatalf("section changed:\n got %s\nwant %s", out, in)
	}
}
