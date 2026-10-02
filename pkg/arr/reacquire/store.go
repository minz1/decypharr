package reacquire

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirrobot01/appendstore"
)

const (
	bindingAttributeArrName  = "arrName"
	bindingManifestKeyPrefix = "arr-binding-manifest:"
	bindingPageKeyPrefix     = "arr-binding-page:"
	bindingDeltaKeyPrefix    = "arr-binding-delta:"
	bindingSnapshotKeyPrefix = "arr-binding-snapshot:"
	bindingSnapshotVersion   = 1
	// bindingPageSize bounds one stored row. A library can hold hundreds of
	// thousands of bindings, and a single row would be encoded, held, and
	// written whole.
	bindingPageSize     = 5000
	jobAttributeArrName = "arrName"
	jobAttributeStatus  = "status"
	// arrStoreCacheSize is how many rows each reacquire store keeps decoded.
	arrStoreCacheSize = 1000
	// arrStoreCompactionThreshold is the dead-row ratio that triggers compaction.
	arrStoreCompactionThreshold = 0.5
)

type bindingRepositoryStore interface {
	ForEach(func(string, []byte) error) error
	Put(string, []byte, *appendstore.PutOptions) error
	Delete(string) error
	Sync() error
	Close() error
}

// BindingRepository persists bindings as a paged snapshot per Arr plus a delta
// row per targeted change. A full reconciliation writes the pages and then a
// manifest naming them, which is the commit point; single upserts write one
// delta row, because rewriting the snapshot per file does not scale.
type BindingRepository struct {
	mu     sync.RWMutex
	store  bindingRepositoryStore
	state  bindingRepositoryState
	loaded bool
}

// bindingManifest names the pages that make up one Arr's committed snapshot.
// It is written last, so a crash mid-write leaves the previous generation.
type bindingManifest struct {
	Version    int    `json:"version"`
	ArrName    string `json:"arrName"`
	Generation uint64 `json:"generation"`
	Pages      int    `json:"pages"`
	Bindings   int    `json:"bindings"`
}

type bindingPage struct {
	Version    int       `json:"version"`
	ArrName    string    `json:"arrName"`
	Generation uint64    `json:"generation"`
	Page       int       `json:"page"`
	Bindings   []Binding `json:"bindings"`
}

// bindingSnapshot is the single-row format paged snapshots replaced. It is
// still read so an index written by an older build survives an upgrade.
type bindingSnapshot struct {
	Version    int       `json:"version"`
	ArrName    string    `json:"arrName"`
	Generation uint64    `json:"generation"`
	Bindings   []Binding `json:"bindings"`
}

// bindingDelta is one change made since its arr.Arr's snapshot was written.
type bindingDelta struct {
	Version     int      `json:"version"`
	ArrName     string   `json:"arrName"`
	EntryID     string   `json:"entryId"`
	EntryFileID string   `json:"entryFileId"`
	Generation  uint64   `json:"generation,omitzero"`
	Deleted     bool     `json:"deleted,omitempty"`
	Binding     *Binding `json:"binding,omitempty"`
}

// bindingRepositoryState indexes what is on disk. It never retains binding
// payloads: the in-memory Index already holds those.
type bindingRepositoryState struct {
	owners      map[entryFileKey]string
	generations map[string]uint64
	deltas      map[entryFileKey]struct{}
	legacy      map[string]map[entryFileKey]struct{}
	stored      map[string][]storedPage
}

// storedPage is one snapshot row on disk, kept so the generation that replaces
// it can delete it.
type storedPage struct {
	key        string
	generation uint64
}

func OpenBindingRepository(path string) (*BindingRepository, error) {
	store, err := openArrStore(path, []string{bindingAttributeArrName})
	if err != nil {
		return nil, fmt.Errorf("open arr binding repository: %w", err)
	}
	return &BindingRepository{store: store}, nil
}

func (r *BindingRepository) LoadAll() ([]Binding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	bindings, state, err := r.scanLocked()
	if err != nil {
		return nil, err
	}
	r.state = state
	r.loaded = true
	sortBindings(bindings)
	return bindings, nil
}

func (r *BindingRepository) Save(binding Binding) error {
	if err := binding.validate(); err != nil {
		return fmt.Errorf("save arr binding: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	state, err := r.stateLocked()
	if err != nil {
		return err
	}
	key := entryFileKey{entryID: binding.EntryID, fileID: binding.EntryFileID}
	if owner, exists := state.owners[key]; exists && owner != binding.ArrName {
		return fmt.Errorf("save arr binding: managed file already belongs to arr %q", owner)
	}

	stored := cloneBinding(binding)
	delta := bindingDelta{
		Version:     bindingSnapshotVersion,
		ArrName:     binding.ArrName,
		EntryID:     binding.EntryID,
		EntryFileID: binding.EntryFileID,
		Generation:  binding.Generation,
		Binding:     &stored,
	}
	if persistDeltaLockedErr := r.persistDeltaLocked(delta); persistDeltaLockedErr != nil {
		return persistDeltaLockedErr
	}
	state.owners[key] = binding.ArrName
	state.deltas[key] = struct{}{}
	state.generations[binding.ArrName] = max(state.generations[binding.ArrName], binding.Generation)
	return nil
}

func (r *BindingRepository) Delete(entryID, fileID string) error {
	if entryID == "" || fileID == "" {
		return errors.New("entry ID and file ID are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	state, err := r.stateLocked()
	if err != nil {
		return err
	}
	key := entryFileKey{entryID: entryID, fileID: fileID}
	owner, exists := state.owners[key]
	if !exists {
		return nil
	}
	delta := bindingDelta{
		Version:     bindingSnapshotVersion,
		ArrName:     owner,
		EntryID:     entryID,
		EntryFileID: fileID,
		Generation:  state.generations[owner],
		Deleted:     true,
	}
	if persistDeltaLockedErr := r.persistDeltaLocked(delta); persistDeltaLockedErr != nil {
		return persistDeltaLockedErr
	}
	delete(state.owners, key)
	state.deltas[key] = struct{}{}
	return nil
}

func (r *BindingRepository) ReplaceArrGeneration(arrName string, generation uint64, bindings []Binding) error {
	prepared, err := prepareGeneration(arrName, generation, bindings)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	state, err := r.stateLocked()
	if err != nil {
		return err
	}
	for _, binding := range prepared {
		bindingKey := entryFileKey{entryID: binding.EntryID, fileID: binding.EntryFileID}
		if owner, exists := state.owners[bindingKey]; exists && owner != arrName {
			return fmt.Errorf("replace arr bindings: managed file already belongs to arr %q", owner)
		}
	}

	options := &appendstore.PutOptions{Attributes: map[string]string{
		bindingAttributeArrName: arrName,
		"generation":            strconv.FormatUint(generation, 10),
	}}
	written, err := r.writePagesLocked(arrName, generation, prepared, options)
	if err != nil {
		return err
	}

	manifest := bindingManifest{
		Version:    bindingSnapshotVersion,
		ArrName:    arrName,
		Generation: generation,
		Pages:      len(written),
		Bindings:   len(prepared),
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode arr binding manifest: %w", err)
	}
	if putErr := r.store.Put(bindingManifestStoreKey(arrName), data, options); putErr != nil {
		return fmt.Errorf("persist arr binding manifest: %w", putErr)
	}
	if syncErr := r.store.Sync(); syncErr != nil {
		r.invalidateLocked()
		return fmt.Errorf("sync arr binding manifest: %w", syncErr)
	}

	// The generation is committed from here on, so the rows it replaces are
	// dropped. A crash in between leaves rows the loader ignores.
	r.dropSupersededRowsLocked(state, arrName, prepared, written)
	state.stored[arrName] = written
	state.generations[arrName] = generation
	return nil
}

// prepareGeneration stamps bindings with arrName and generation, validates
// them, and sorts them into page order.
func prepareGeneration(arrName string, generation uint64, bindings []Binding) ([]Binding, error) {
	if arrName == "" {
		return nil, errors.New("arr name is required")
	}
	prepared := make([]Binding, len(bindings))
	for i, binding := range bindings {
		if binding.ArrName != "" && binding.ArrName != arrName {
			return nil, fmt.Errorf("binding %q belongs to arr %q", binding.EntryFileID, binding.ArrName)
		}
		binding.ArrName = arrName
		binding.Generation = generation
		if err := binding.validate(); err != nil {
			return nil, fmt.Errorf("replace arr bindings: %w", err)
		}
		prepared[i] = cloneBinding(binding)
	}
	if err := validateUniqueArrFiles(prepared); err != nil {
		return nil, err
	}
	if err := validateUniqueManagedFiles(prepared); err != nil {
		return nil, fmt.Errorf("replace arr bindings: %w", err)
	}
	sortBindings(prepared)
	return prepared, nil
}

// writePagesLocked writes a generation's pages and syncs them, so that a
// manifest written afterwards never names a page that is not on disk.
func (r *BindingRepository) writePagesLocked(
	arrName string,
	generation uint64,
	prepared []Binding,
	options *appendstore.PutOptions,
) ([]storedPage, error) {
	written := make([]storedPage, 0, len(prepared)/bindingPageSize+1)
	index := 0
	for chunk := range slices.Chunk(prepared, bindingPageSize) {
		page := bindingPage{
			Version:    bindingSnapshotVersion,
			ArrName:    arrName,
			Generation: generation,
			Page:       index,
			Bindings:   chunk,
		}
		data, marshalErr := json.Marshal(page)
		if marshalErr != nil {
			return nil, fmt.Errorf("encode arr binding page: %w", marshalErr)
		}
		key := bindingPageStoreKey(arrName, generation, index)
		if putErr := r.store.Put(key, data, options); putErr != nil {
			return nil, fmt.Errorf("persist arr binding page: %w", putErr)
		}
		written = append(written, storedPage{key: key, generation: generation})
		index++
	}
	if syncErr := r.store.Sync(); syncErr != nil {
		r.invalidateLocked()
		return nil, fmt.Errorf("sync arr binding pages: %w", syncErr)
	}
	return written, nil
}

func (r *BindingRepository) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.store.Close()
}

func (r *BindingRepository) stateLocked() (*bindingRepositoryState, error) {
	if r.loaded {
		return &r.state, nil
	}
	_, state, err := r.scanLocked()
	if err != nil {
		return nil, err
	}
	r.state = state
	r.loaded = true
	return &r.state, nil
}

func (r *BindingRepository) invalidateLocked() {
	r.state = bindingRepositoryState{}
	r.loaded = false
}

// scanLocked reads the store once and merges the three row kinds: a snapshot is
// authoritative for its arr.Arr, deltas written after it win per managed file, and
// legacy rows only apply to an arr.Arr that has no snapshot yet.
func (r *BindingRepository) scanLocked() ([]Binding, bindingRepositoryState, error) {
	rows := scannedRows{
		manifests: make(map[string]bindingManifest),
		pages:     make(map[string][]bindingPage),
		deltas:    make(map[entryFileKey]bindingDelta),
		legacy:    make(map[string][]Binding),
		stored:    make(map[string][]storedPage),
	}
	if err := r.store.ForEach(rows.add); err != nil {
		return nil, bindingRepositoryState{}, err
	}
	return rows.merge()
}

// scannedRows holds every row of the binding store, decoded and grouped by kind.
type scannedRows struct {
	manifests map[string]bindingManifest
	pages     map[string][]bindingPage
	deltas    map[entryFileKey]bindingDelta
	legacy    map[string][]Binding
	stored    map[string][]storedPage
}

// add decodes one row by its key prefix. Any row that fails to decode or
// validate fails the whole scan.
func (rows *scannedRows) add(key string, value []byte) error {
	switch {
	case strings.HasPrefix(key, bindingManifestKeyPrefix):
		manifest, err := decodeRow(key, value, "manifest", validateBindingManifest)
		if err != nil {
			return err
		}
		rows.manifests[manifest.ArrName] = manifest
	case strings.HasPrefix(key, bindingPageKeyPrefix):
		page, err := decodeRow(key, value, "page", validateBindingPage)
		if err != nil {
			return err
		}
		rows.addPage(key, page)
	case strings.HasPrefix(key, bindingSnapshotKeyPrefix):
		// The single-row format an older build wrote.
		snapshot, err := decodeRow(key, value, "snapshot", validateBindingSnapshot)
		if err != nil {
			return err
		}
		rows.addPage(key, bindingPage{
			Version:    snapshot.Version,
			ArrName:    snapshot.ArrName,
			Generation: snapshot.Generation,
			Bindings:   snapshot.Bindings,
		})
	case strings.HasPrefix(key, bindingDeltaKeyPrefix):
		delta, err := decodeRow(key, value, "delta", validateBindingDelta)
		if err != nil {
			return err
		}
		rows.deltas[entryFileKey{entryID: delta.EntryID, fileID: delta.EntryFileID}] = delta
	default:
		binding, err := decodeRow(key, value, "legacy", func(_ string, binding Binding) error {
			return binding.validate()
		})
		if err != nil {
			return err
		}
		rows.legacy[binding.ArrName] = append(rows.legacy[binding.ArrName], binding)
	}
	return nil
}

// decodeRow unmarshals and validates one stored row of the named kind.
func decodeRow[T any](key string, value []byte, kind string, validate func(string, T) error) (T, error) {
	var row T
	if err := json.Unmarshal(value, &row); err != nil {
		return row, fmt.Errorf("decode arr binding %s %q: %w", kind, key, err)
	}
	if err := validate(key, row); err != nil {
		return row, fmt.Errorf("decode arr binding %s %q: %w", kind, key, err)
	}
	return row, nil
}

func (rows *scannedRows) addPage(key string, page bindingPage) {
	rows.pages[page.ArrName] = append(rows.pages[page.ArrName], page)
	rows.stored[page.ArrName] = append(rows.stored[page.ArrName], storedPage{key: key, generation: page.Generation})
}

// merge resolves the rows into the current bindings and the repository state.
func (rows *scannedRows) merge() ([]Binding, bindingRepositoryState, error) {
	state := bindingRepositoryState{
		owners:      make(map[entryFileKey]string),
		generations: make(map[string]uint64, len(rows.pages)),
		deltas:      make(map[entryFileKey]struct{}, len(rows.deltas)),
		legacy:      make(map[string]map[entryFileKey]struct{}, len(rows.legacy)),
		stored:      rows.stored,
	}
	merged := make(map[entryFileKey]Binding)
	committed := rows.mergeSnapshots(&state, merged)
	rows.mergeLegacy(&state, merged, committed)
	rows.mergeDeltas(&state, merged, committed)

	bindings := make([]Binding, 0, len(merged))
	for key, binding := range merged {
		if owner, exists := state.owners[key]; exists {
			return nil, bindingRepositoryState{}, fmt.Errorf(
				"managed file belongs to both arr %q and %q",
				owner,
				binding.ArrName,
			)
		}
		state.owners[key] = binding.ArrName
		bindings = append(bindings, cloneBinding(binding))
	}
	return bindings, state, nil
}

// mergeSnapshots applies each Arr's committed generation and returns it per Arr.
func (rows *scannedRows) mergeSnapshots(
	state *bindingRepositoryState,
	merged map[entryFileKey]Binding,
) map[string]uint64 {
	committed := make(map[string]uint64, len(rows.pages))
	for arrName, arrPages := range rows.pages {
		generation, ok := committedGeneration(arrName, rows.manifests, arrPages)
		if !ok {
			// Pages with no manifest, or too few for the one on disk: an
			// interrupted write, which the previous generation still covers.
			continue
		}
		committed[arrName] = generation
		state.generations[arrName] = generation
		for _, page := range arrPages {
			if page.Generation != generation {
				continue
			}
			for _, binding := range page.Bindings {
				merged[entryFileKey{entryID: binding.EntryID, fileID: binding.EntryFileID}] = binding
			}
		}
	}
	return committed
}

// mergeLegacy applies per-file rows from before snapshots, for Arrs that have
// no committed snapshot yet.
func (rows *scannedRows) mergeLegacy(
	state *bindingRepositoryState,
	merged map[entryFileKey]Binding,
	committed map[string]uint64,
) {
	for arrName, bindings := range rows.legacy {
		keys := make(map[entryFileKey]struct{}, len(bindings))
		for _, binding := range bindings {
			keys[entryFileKey{entryID: binding.EntryID, fileID: binding.EntryFileID}] = struct{}{}
		}
		state.legacy[arrName] = keys
		if _, superseded := committed[arrName]; superseded {
			continue
		}
		for _, binding := range bindings {
			key := entryFileKey{entryID: binding.EntryID, fileID: binding.EntryFileID}
			merged[key] = binding
			state.generations[arrName] = max(state.generations[arrName], binding.Generation)
		}
	}
}

// mergeDeltas applies targeted changes written after their Arr's snapshot.
func (rows *scannedRows) mergeDeltas(
	state *bindingRepositoryState,
	merged map[entryFileKey]Binding,
	committed map[string]uint64,
) {
	for key, delta := range rows.deltas {
		state.deltas[key] = struct{}{}
		if delta.Generation < committed[delta.ArrName] {
			continue // superseded by a generation written after this delta
		}
		if delta.Deleted {
			delete(merged, key)
			continue
		}
		merged[key] = *delta.Binding
		state.generations[delta.ArrName] = max(state.generations[delta.ArrName], delta.Generation)
	}
}

// committedGeneration reports the newest generation whose pages are all on
// disk. A manifest names how many pages its generation has; a snapshot an
// older build wrote is a single page with no manifest.
func committedGeneration(arrName string, manifests map[string]bindingManifest, pages []bindingPage) (uint64, bool) {
	if manifest, ok := manifests[arrName]; ok {
		found := 0
		for _, page := range pages {
			if page.Generation == manifest.Generation {
				found++
			}
		}
		if found != manifest.Pages {
			return 0, false
		}
		return manifest.Generation, true
	}

	newest := uint64(0)
	found := false
	for _, page := range pages {
		if page.Page != 0 {
			continue // a paged generation is only valid with its manifest
		}
		if !found || page.Generation > newest {
			newest = page.Generation
			found = true
		}
	}
	return newest, found
}

func (r *BindingRepository) persistDeltaLocked(delta bindingDelta) error {
	key := bindingDeltaStoreKey(delta.EntryID, delta.EntryFileID)
	if err := validateBindingDelta(key, delta); err != nil {
		return fmt.Errorf("persist arr binding delta: %w", err)
	}
	data, err := json.Marshal(delta)
	if err != nil {
		return fmt.Errorf("encode arr binding delta: %w", err)
	}
	options := &appendstore.PutOptions{Attributes: map[string]string{
		bindingAttributeArrName: delta.ArrName,
	}}
	if putErr := r.store.Put(key, data, options); putErr != nil {
		return fmt.Errorf("persist arr binding delta: %w", putErr)
	}
	if syncErr := r.store.Sync(); syncErr != nil {
		r.invalidateLocked()
		return fmt.Errorf("sync arr binding delta: %w", syncErr)
	}
	return nil
}

// dropSupersededRowsLocked removes the pages, deltas, and legacy rows a fresh
// generation replaces. Failures are not fatal: the loader ignores stale rows.
func (r *BindingRepository) dropSupersededRowsLocked(
	state *bindingRepositoryState,
	arrName string,
	bindings []Binding,
	written []storedPage,
) {
	// A replace that reuses the committed generation overwrites the same page
	// keys; deleting those would drop the pages the new manifest names.
	for _, page := range state.stored[arrName] {
		if !slices.Contains(written, page) {
			_ = r.store.Delete(page.key)
		}
	}
	delete(state.stored, arrName)

	for key := range state.legacy[arrName] {
		if err := r.store.Delete(bindingStoreKey(key.entryID, key.fileID)); err == nil {
			delete(state.owners, key)
		}
	}
	delete(state.legacy, arrName)

	for key := range state.deltas {
		if state.owners[key] != arrName {
			continue
		}
		if err := r.store.Delete(bindingDeltaStoreKey(key.entryID, key.fileID)); err != nil {
			continue
		}
		delete(state.deltas, key)
	}
	for key, owner := range state.owners {
		if owner == arrName {
			delete(state.owners, key)
		}
	}
	for _, binding := range bindings {
		key := entryFileKey{entryID: binding.EntryID, fileID: binding.EntryFileID}
		if _, stale := state.deltas[key]; stale {
			if err := r.store.Delete(bindingDeltaStoreKey(key.entryID, key.fileID)); err == nil {
				delete(state.deltas, key)
			}
		}
		state.owners[key] = arrName
	}
}

func validateBindingManifest(key string, manifest bindingManifest) error {
	switch {
	case manifest.Version != bindingSnapshotVersion:
		return fmt.Errorf("unsupported version %d", manifest.Version)
	case manifest.ArrName == "":
		return errors.New("arr name is required")
	case key != bindingManifestStoreKey(manifest.ArrName):
		return errors.New("manifest key does not match arr name")
	case manifest.Pages < 0 || manifest.Bindings < 0:
		return errors.New("manifest counts are negative")
	default:
		return nil
	}
}

func validateBindingPage(key string, page bindingPage) error {
	switch {
	case page.Version != bindingSnapshotVersion:
		return fmt.Errorf("unsupported version %d", page.Version)
	case page.ArrName == "":
		return errors.New("arr name is required")
	case key != bindingPageStoreKey(page.ArrName, page.Generation, page.Page):
		return errors.New("page key does not match its arr, generation, and index")
	}
	return validatePageBindings(page.ArrName, page.Generation, page.Bindings)
}

func validateBindingSnapshot(key string, snapshot bindingSnapshot) error {
	switch {
	case snapshot.Version != bindingSnapshotVersion:
		return fmt.Errorf("unsupported version %d", snapshot.Version)
	case snapshot.ArrName == "":
		return errors.New("arr name is required")
	case key != bindingSnapshotStoreKey(snapshot.ArrName):
		return errors.New("snapshot key does not match arr name")
	}
	if err := validatePageBindings(snapshot.ArrName, snapshot.Generation, snapshot.Bindings); err != nil {
		return err
	}
	return validateUniqueManagedFiles(snapshot.Bindings)
}

func validatePageBindings(arrName string, generation uint64, bindings []Binding) error {
	for _, binding := range bindings {
		if err := binding.validate(); err != nil {
			return err
		}
		if binding.ArrName != arrName {
			return fmt.Errorf("binding belongs to arr %q", binding.ArrName)
		}
		if binding.Generation > generation {
			return fmt.Errorf("binding generation %d exceeds generation %d", binding.Generation, generation)
		}
	}
	return nil
}

func validateUniqueManagedFiles(bindings []Binding) error {
	seen := make(map[entryFileKey]struct{}, len(bindings))
	for _, binding := range bindings {
		key := entryFileKey{entryID: binding.EntryID, fileID: binding.EntryFileID}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("duplicate managed file %q/%q", binding.EntryID, binding.EntryFileID)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateBindingDelta(key string, delta bindingDelta) error {
	switch {
	case delta.Version != bindingSnapshotVersion:
		return fmt.Errorf("unsupported version %d", delta.Version)
	case delta.ArrName == "":
		return errors.New("arr name is required")
	case key != bindingDeltaStoreKey(delta.EntryID, delta.EntryFileID):
		return errors.New("delta key does not match the managed file")
	case delta.Deleted:
		return nil
	case delta.Binding == nil:
		return errors.New("delta binding is required")
	}
	if err := delta.Binding.validate(); err != nil {
		return err
	}
	if delta.Binding.ArrName != delta.ArrName ||
		delta.Binding.EntryID != delta.EntryID ||
		delta.Binding.EntryFileID != delta.EntryFileID {
		return errors.New("delta binding identity does not match the delta")
	}
	return nil
}

func openArrStore(path string, indexedFields []string) (*appendstore.Store, error) {
	if path == "" {
		return nil, errors.New("database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	return appendstore.Open(path, appendstore.Options{
		CacheSize:           arrStoreCacheSize,
		SyncInterval:        time.Second,
		CompactionThreshold: arrStoreCompactionThreshold,
		AutoCompact:         true,
		IndexedFields:       indexedFields,
	})
}

func bindingStoreKey(entryID, fileID string) string {
	hash := sha256.New()
	hash.Write([]byte(entryID))
	hash.Write([]byte{0})
	hash.Write([]byte(fileID))
	return hex.EncodeToString(hash.Sum(nil))
}

func bindingDeltaStoreKey(entryID, fileID string) string {
	return bindingDeltaKeyPrefix + bindingStoreKey(entryID, fileID)
}

func bindingSnapshotStoreKey(arrName string) string {
	return bindingSnapshotKeyPrefix + arrNameHash(arrName)
}

func bindingManifestStoreKey(arrName string) string {
	return bindingManifestKeyPrefix + arrNameHash(arrName)
}

func bindingPageStoreKey(arrName string, generation uint64, page int) string {
	return fmt.Sprintf("%s%s:%d:%d", bindingPageKeyPrefix, arrNameHash(arrName), generation, page)
}

func arrNameHash(arrName string) string {
	hash := sha256.Sum256([]byte(arrName))
	return hex.EncodeToString(hash[:])
}

func deleteStoreKey(store *appendstore.Store, key string) error {
	if err := store.Delete(key); err != nil && !errors.Is(err, appendstore.ErrKeyNotFound) {
		return err
	}
	return nil
}
