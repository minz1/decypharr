package strm

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/fsutil"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// maxStrmRead bounds .strm content reads; canonical URLs are far smaller.
const maxStrmRead = 1024

// Reconciler maintains the STRM export tree and its sidecar files.
// It removes stale files only when their URLs identify them as our exports.
type Reconciler struct {
	config     *config.Store
	storage    *storage.Storage
	ctx        context.Context
	openStream func(context.Context, *storage.Entry, string) (io.ReadCloser, error)
	logger     zerolog.Logger
	sweepMu    sync.Mutex
}

// NewReconciler creates the export service with its storage and stream
// source. STRM settings are read live from cfg.
func NewReconciler(
	ctx context.Context,
	cfg *config.Store,
	store *storage.Storage,
	openStream func(context.Context, *storage.Entry, string) (io.ReadCloser, error),
	logger zerolog.Logger,
) *Reconciler {
	return &Reconciler{ctx: ctx, config: cfg, storage: store, openStream: openStream,
		logger: logger.With().Str("component", "strm").Logger()}
}

// Report is the outcome of a reconcile pass.
type Report struct {
	Entries  int      `json:"entries"`
	Verified int      `json:"verified"`
	Written  int      `json:"written"`
	Sidecars int      `json:"sidecars"`
	Deleted  int      `json:"deleted"`
	Errors   []string `json:"errors,omitempty"`
}

func (r *Report) addError(err error) {
	r.Errors = append(r.Errors, err.Error())
}

type strmTarget struct {
	path    string
	content string
}

// entryDir returns the entry's folder inside the export tree; mirrors the
// __all__ mount layout.
func entryDir(cfg *config.Config, entry *storage.Entry) string {
	return filepath.Join(cfg.Strm.Path, entry.GetFolder(cfg.FolderNaming))
}

// desired returns the .strm files and sidecar downloads an entry should have.
// Files whose provider name is not a single path element are reported and
// skipped: their path would leave the entry's folder.
func (s *Reconciler) desired(entry *storage.Entry, rep *Report) ([]strmTarget, []*storage.File) {
	cfg := s.config.Get()
	base := BaseURL(cfg)
	dir := entryDir(cfg, entry)
	maxSidecar := cfg.Strm.SidecarMaxBytes()

	var targets []strmTarget
	var sidecars []*storage.File
	for _, f := range entry.GetActiveFiles() {
		switch {
		case utils.IsVideoFile(f.Name):
			path, err := fsutil.JoinName(dir, FileName(f.Name, cfg.Strm.KeepMediaExtension))
			if err != nil {
				rep.addError(fmt.Errorf("strm for %s: %w", entry.InfoHash, err))
				continue
			}
			targets = append(targets, strmTarget{
				path:    path,
				content: FileURL(base, cfg.Strm.Secret, entry.InfoHash, f.ID, f.Name),
			})
		case cfg.Strm.SidecarsEnabled() && IsSidecar(f.Name) && f.Size > 0 && f.Size <= maxSidecar:
			sidecars = append(sidecars, f)
		}
	}
	return targets, sidecars
}

// active reports whether STRM export is on. A nil Reconciler is inactive.
func (s *Reconciler) active() bool {
	return s != nil && s.config.Get().Strm.Active()
}

// SyncEntryAsync reconciles one entry's export folder in the background —
// the post-download and entry-updated trigger. Only entries present in main
// storage are exported; their URLs must resolve.
func (s *Reconciler) SyncEntryAsync(entry *storage.Entry) {
	if !s.active() {
		return
	}
	go func() {
		if ok, _ := s.storage.Exists(entry.InfoHash); !ok {
			return
		}
		rep := &Report{}
		s.syncEntry(s.ctx, entry, rep)
		for _, e := range rep.Errors {
			s.logger.Warn().Str("entry", entry.Name).Msg("strm sync: " + e)
		}
	}()
}

// syncEntry reconciles one entry's folder: (re)write desired .strm files,
// remove stale ones this entry owns, download missing sidecars. Returns the
// desired targets so sweeps know which paths are accounted for.
func (s *Reconciler) syncEntry(ctx context.Context, entry *storage.Entry, rep *Report) []strmTarget {
	if len(entry.Files) == 0 {
		return nil
	}

	// Backfill stable file IDs for entries stored before IDs existed;
	// AddOrUpdate assigns and persists them.
	for _, f := range entry.Files {
		if f.ID == "" {
			if err := s.storage.AddOrUpdate(entry); err != nil {
				rep.addError(fmt.Errorf("assign file ids for %s: %w", entry.Name, err))
				return nil
			}
			break
		}
	}

	rep.Entries++
	targets, sidecars := s.desired(entry, rep)
	for _, t := range targets {
		current, err := readStrm(t.path)
		if err == nil && current == t.content {
			rep.Verified++
			continue
		}
		if writeStrmErr := writeStrm(s.config.Get(), t.path, t.content); writeStrmErr != nil {
			rep.addError(writeStrmErr)
			continue
		}
		rep.Written++
	}
	s.removeStale(entry, targets, rep)
	for _, f := range sidecars {
		if ctx.Err() != nil {
			break
		}
		s.syncSidecar(ctx, entry, f, rep)
	}
	return targets
}

// removeStale deletes .strm files in the entry's folder that carry this
// entry's infohash but are no longer desired (renamed by a repair, naming
// config changed). Other entries may share the folder name; their files are
// left alone.
func (s *Reconciler) removeStale(entry *storage.Entry, targets []strmTarget, rep *Report) {
	keep := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		keep[t.path] = struct{}{}
	}
	for _, path := range entryStrmFiles(entryDir(s.config.Get(), entry), entry.InfoHash) {
		if _, ok := keep[path]; ok {
			continue
		}
		if removeErr := os.Remove(path); removeErr != nil {
			rep.addError(removeErr)
			continue
		}
		rep.Deleted++
	}
}

// entryStrmFiles lists the .strm files under dir that point at infohash.
// Unreadable paths are skipped: they are not provably ours.
func entryStrmFiles(dir, infohash string) []string {
	var files []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if owner, ok := strmOwner(path, d, walkErr); ok && owner == infohash {
			files = append(files, path)
		}
		return nil
	})
	return files
}

// strmOwner returns the infohash a walked .strm file of ours points at; ok is
// false for directories, other files, foreign URLs and walk errors.
func strmOwner(path string, d fs.DirEntry, walkErr error) (string, bool) {
	if walkErr != nil || d.IsDir() || !strings.EqualFold(filepath.Ext(path), ".strm") {
		return "", false
	}
	content, err := readStrm(path)
	if err != nil {
		return "", false
	}
	infohash, _, ok := ParseURL(content)
	return infohash, ok
}

func (s *Reconciler) syncSidecar(ctx context.Context, entry *storage.Entry, file *storage.File, rep *Report) {
	dest, err := fsutil.JoinName(entryDir(s.config.Get(), entry), file.Name)
	if err != nil {
		rep.addError(fmt.Errorf("sidecar for %s: %w", entry.InfoHash, err))
		return
	}
	if fi, statErr := os.Stat(dest); statErr == nil && fi.Size() == file.Size {
		return
	}
	if err = s.downloadSidecar(ctx, entry, file, dest); err != nil {
		rep.addError(fmt.Errorf("sidecar %s: %w", file.Name, err))
		return
	}
	rep.Sidecars++
}

func (s *Reconciler) downloadSidecar(ctx context.Context, entry *storage.Entry, file *storage.File, dest string) error {
	stream, err := s.openStream(ctx, entry, file.Name)
	if err != nil {
		return err
	}
	defer stream.Close()

	// The export tree is read by media servers running as other users.
	cfg := s.config.Get()
	if mkdirErr := fsutil.MkdirShared(filepath.Dir(dest), cfg.SharedDirModeValue()); mkdirErr != nil {
		return mkdirErr
	}
	tmp := dest + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, cfg.SharedFileModeValue())
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(stream, file.Size))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != file.Size {
		err = fmt.Errorf("short download: %d of %d bytes", n, file.Size)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// Sweep reconciles the whole export tree: every entry's folder is synced,
// then any .strm left in the tree that is ours but no longer desired —
// deleted entries, renamed files, stale folder names — is removed, pruning
// directories that become empty.
func (s *Reconciler) Sweep(ctx context.Context) (*Report, error) {
	if !s.active() {
		return nil, fmt.Errorf("strm is disabled or has no path configured")
	}
	cfg := s.config.Get()

	s.sweepMu.Lock()
	defer s.sweepMu.Unlock()

	rep := &Report{}
	entries, err := s.storage.List(nil)
	if err != nil {
		return nil, err
	}

	owned := make(map[string]struct{})
	for _, e := range entries {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return rep, ctxErr
		}
		for _, t := range s.syncEntry(ctx, e, rep) {
			owned[t.path] = struct{}{}
		}
	}

	var stale []string
	_ = filepath.WalkDir(cfg.Strm.Path, func(path string, d fs.DirEntry, walkErr error) error {
		if _, isOwned := owned[path]; !isOwned {
			if _, ours := strmOwner(path, d, walkErr); ours {
				stale = append(stale, path)
			}
		}
		return ctx.Err()
	})
	for _, path := range stale {
		if removeErr := os.Remove(path); removeErr != nil {
			rep.addError(removeErr)
			continue
		}
		rep.Deleted++
		pruneEmptyDirs(filepath.Dir(path), cfg.Strm.Path)
	}

	return rep, nil
}

// SweepAsync runs a background sweep — the regenerate, config-change, and
// startup trigger. A no-op while strm is disabled.
func (s *Reconciler) SweepAsync(reason string) {
	if !s.active() {
		return
	}
	go func() {
		rep, err := s.Sweep(s.ctx)
		if err != nil {
			s.logger.Warn().Err(err).Str("reason", reason).Msg("strm sweep failed")
			return
		}
		s.logger.Info().
			Str("reason", reason).
			Int("entries", rep.Entries).
			Int("written", rep.Written).
			Int("verified", rep.Verified).
			Int("sidecars", rep.Sidecars).
			Int("deleted", rep.Deleted).
			Int("errors", len(rep.Errors)).
			Msg("strm sweep complete")
	}()
}

// RemoveEntryAsync deletes an entry's .strm files right after the entry is
// deleted, so its folder doesn't linger until the next sweep. Only files
// carrying the entry's infohash are removed.
func (s *Reconciler) RemoveEntryAsync(entry *storage.Entry) {
	if !s.active() {
		return
	}
	cfg := s.config.Get()
	go func() {
		dir := entryDir(cfg, entry)
		for _, path := range entryStrmFiles(dir, entry.InfoHash) {
			_ = os.Remove(path)
		}
		// Sidecars carry no signature; remove them by name while we still
		// know the entry's file list.
		for _, f := range entry.Files {
			if !IsSidecar(f.Name) {
				continue
			}
			if path, err := fsutil.JoinName(dir, f.Name); err == nil {
				_ = os.Remove(path)
			}
		}
		pruneEmptyDirs(dir, cfg.Strm.Path)
	}()
}

// pruneEmptyDirs removes empty directories from dir up to (excluding) root.
func pruneEmptyDirs(dir, root string) {
	root = filepath.Clean(root)
	for dir = filepath.Clean(dir); dir != root && strings.HasPrefix(dir, root+string(filepath.Separator)); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			return // not empty or gone
		}
	}
}

func readStrm(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxStrmRead))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// writeStrm writes one .strm file with the shared modes: the export tree is
// read by media servers running as other users.
func writeStrm(cfg *config.Config, path, content string) error {
	if err := fsutil.MkdirShared(filepath.Dir(path), cfg.SharedDirModeValue()); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), cfg.SharedFileModeValue())
}
