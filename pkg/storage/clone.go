package storage

import (
	"maps"
	"slices"
)

// Clone returns a deep copy of e: no map, slice or pointer is shared with it.
func (e *Entry) Clone() *Entry {
	if e == nil {
		return nil
	}
	c := *e
	c.Tags = slices.Clone(e.Tags)
	c.Grab = slices.Clone(e.Grab)
	c.CompletedAt = clonePtr(e.CompletedAt)
	c.ImportedAt = clonePtr(e.ImportedAt)
	c.LastErrorTime = clonePtr(e.LastErrorTime)
	c.Files = cloneMap(e.Files, func(f *File) *File {
		fc := *f
		fc.ByteRange = clonePtr(f.ByteRange)
		return &fc
	})
	c.Providers = cloneMap(e.Providers, func(p *ProviderEntry) *ProviderEntry {
		pc := *p
		pc.RemovedAt = clonePtr(p.RemovedAt)
		pc.DownloadedAt = clonePtr(p.DownloadedAt)
		pc.Files = cloneMap(p.Files, clonePtr[ProviderFile])
		return &pc
	})
	return &c
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneMap[V any](m map[string]*V, clone func(*V) *V) map[string]*V {
	if m == nil {
		return nil
	}
	out := maps.Clone(m)
	for k, v := range out {
		if v != nil {
			out[k] = clone(v)
		}
	}
	return out
}
