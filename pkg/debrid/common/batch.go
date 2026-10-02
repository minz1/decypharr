package common

// HashBatches splits hashes into consecutive windows of size and drops empty
// hashes from each window. Windows left empty are omitted.
func HashBatches(hashes []string, size int) [][]string {
	var batches [][]string
	for i := 0; i < len(hashes); i += size {
		window := hashes[i:min(i+size, len(hashes))]
		batch := make([]string, 0, len(window))
		for _, hash := range window {
			if hash != "" {
				batch = append(batch, hash)
			}
		}
		if len(batch) > 0 {
			batches = append(batches, batch)
		}
	}
	return batches
}
