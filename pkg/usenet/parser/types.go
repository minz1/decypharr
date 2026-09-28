package parser

// GetName returns the file name.
func (r *RARFileEntry) GetName() string { return r.Name }

// GetSize returns the uncompressed size.
func (r *RARFileEntry) GetSize() int64 { return r.UncompressedSize }

// IsStreamable returns true if file is stored without compression.
func (r *RARFileEntry) IsStreamable() bool { return r.IsStored }
