package manager

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sirrobot01/appendstore"
)

type referenceScan struct {
	Links      int                 // symlinks seen under the roots
	Referenced map[string]struct{} // infohashes served to at least one link
	Dangling   int                 // well-formed mount links with no index match
}

type lookupFunc func(folder, file string) (string, bool, error)

const maxLinkHops = 8

func (m *Manager) lookupServedEntry(folder, file string) (string, bool, error) {
	item, err := m.storage.GetEntryItem(folder)
	if errors.Is(err, appendstore.ErrKeyNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read name index %q: %w", folder, err)
	}
	if item == nil {
		return "", false, nil
	}
	f := item.Files[file]
	if f == nil || f.Deleted {
		return "", false, nil
	}
	return f.InfoHash, true, nil
}

func scanReferences(roots []string, mountPath string, lookup lookupFunc) (referenceScan, error) {
	mount := filepath.Clean(mountPath)
	if !filepath.IsAbs(mount) || mount == "/" {
		return referenceScan{}, fmt.Errorf("mount path %q must be absolute and not /", mountPath)
	}
	res := referenceScan{Referenced: map[string]struct{}{}}
	for _, root := range roots {
		physical, err := filepath.EvalSymlinks(root)
		if err != nil {
			return referenceScan{}, fmt.Errorf("scan %s: %w", root, err)
		}
		err = filepath.WalkDir(physical, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type()&fs.ModeSymlink == 0 {
				return nil
			}
			res.Links++
			return res.addLink(path, mount, lookup)
		})
		if err != nil {
			return referenceScan{}, fmt.Errorf("scan %s: %w", root, err)
		}
	}
	return res, nil
}

func (r *referenceScan) addLink(link, mount string, lookup lookupFunc) error {
	path := link
	for hop := 0; hop <= maxLinkHops; hop++ {
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		target = filepath.Clean(target)
		if target == mount || strings.HasPrefix(target, mount+"/") {
			return r.addMountTarget(link, target, mount, lookup)
		}
		info, err := os.Lstat(target)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			return nil
		}
		path = target
	}
	return fmt.Errorf("link chain too long: %s", link)
}

func (r *referenceScan) addMountTarget(link, target, mount string, lookup lookupFunc) error {
	rest, ok := strings.CutPrefix(target, filepath.Join(mount, "__all__")+"/")
	folder, file, found := strings.Cut(rest, "/")
	if !ok || !found || folder == "" || file == "" || strings.Contains(file, "/") {
		return fmt.Errorf("unparseable link into mount: %s -> %s", link, target)
	}
	hash, served, err := lookup(folder, file)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", link, err)
	}
	if served {
		r.Referenced[hash] = struct{}{}
	} else {
		r.Dangling++
	}
	return nil
}
