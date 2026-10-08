package storage

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sirrobot01/appendstore"
	"google.golang.org/protobuf/proto"
)

// AddQueue adds an entry to the queue.
func (s *Storage) AddQueue(entry *Entry) error {
	entry.CreatedAt = time.Now()
	return s.UpdateQueue(entry)
}

// UpdateQueue updates a queued entry.
func (s *Storage) UpdateQueue(entry *Entry) error {
	entry.UpdatedAt = time.Now()
	data, err := proto.Marshal(EntryToProto(entry))
	if err != nil {
		return fmt.Errorf("encode queued entry %q: %w", entry.InfoHash, err)
	}
	if putErr := s.queue.Put(strings.ToLower(entry.InfoHash), data, s.entryPutOptions(entry)); putErr != nil {
		return fmt.Errorf("save queued entry %q: %w", entry.InfoHash, putErr)
	}
	return nil
}

// GetQueued retrieves a queued entry.
func (s *Storage) GetQueued(infohash string) (*Entry, error) {
	key := strings.ToLower(infohash)
	data, err := s.queue.Get(key)
	if err != nil {
		return nil, fmt.Errorf("read queued entry %q: %w", key, err)
	}
	var pb EntryProto
	if unmarshalErr := proto.Unmarshal(data, &pb); unmarshalErr != nil {
		return nil, fmt.Errorf("decode queued entry %q: %w", key, unmarshalErr)
	}
	return ProtoToEntry(&pb), nil
}

// DeleteQueued removes an entry only after its cleanup succeeds.
func (s *Storage) DeleteQueued(infohash string, cleanup func(*Entry) error) error {
	key := strings.ToLower(infohash)
	if cleanup != nil {
		entry, err := s.GetQueued(key)
		if err != nil {
			return err
		}
		if cleanupErr := cleanup(entry); cleanupErr != nil {
			return fmt.Errorf("clean up queued entry %q: %w", key, cleanupErr)
		}
	}
	if err := s.queue.Delete(key); err != nil {
		return fmt.Errorf("delete queued entry %q: %w", key, err)
	}
	return nil
}

// FilterQueued returns matching entries. It returns an error if the scan fails.
func (s *Storage) FilterQueued(filter func(*Entry) bool) ([]*Entry, error) {
	var entries []*Entry
	err := s.queue.ForEach(func(key string, value []byte) error {
		var pb EntryProto
		if err := proto.Unmarshal(value, &pb); err != nil {
			return fmt.Errorf("decode queued entry %q: %w", key, err)
		}
		entry := ProtoToEntry(&pb)
		if filter == nil || filter(entry) {
			entries = append(entries, entry)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan queue: %w", err)
	}
	return entries, nil
}

// FilterQueuedByFolder is FilterQueued for entries whose folder name is one
// of folders. The folder is read from each entry's metadata, so only
// candidates are decoded. An entry stored without a folder, or with a hash
// folder (written under hash naming), cannot be ruled out and is decoded too.
func (s *Storage) FilterQueuedByFolder(folders map[string]struct{}, filter func(*Entry) bool) ([]*Entry, error) {
	var keys []string
	err := s.queue.ForEachMetadata(func(key string, meta *appendstore.Metadata) error {
		folder := meta.Attribute(attributeName)
		if _, ok := folders[folder]; ok || folder == "" || isHashFolder(folder) {
			keys = append(keys, key)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan queue metadata: %w", err)
	}
	var entries []*Entry
	for _, key := range keys {
		entry, getErr := s.GetQueued(key)
		if errors.Is(getErr, appendstore.ErrKeyNotFound) {
			continue // removed since the metadata scan
		}
		if getErr != nil {
			return nil, getErr
		}
		if filter == nil || filter(entry) {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

// isHashFolder reports whether folder looks like an entry ID: 32 or 40 hex
// digits, the folder name under hash naming.
func isHashFolder(folder string) bool {
	if len(folder) != md5HexLen && len(folder) != sha1HexLen {
		return false
	}
	_, err := hex.DecodeString(folder)
	return err == nil
}

// Entry ID lengths: season IDs and NZB IDs are 32 hex digits, torrent info
// hashes 40.
const (
	md5HexLen  = 32
	sha1HexLen = 40
)

// CountQueuedByState counts queued entries without building full entry objects.
func (s *Storage) CountQueuedByState(state TorrentState) int {
	count := 0
	_ = s.queue.ForEach(func(_ string, value []byte) error {
		var pb EntryProto
		if proto.Unmarshal(value, &pb) == nil && pb.GetState() == string(state) {
			count++
		}
		return nil
	})
	return count
}

// DeleteWhereQueued deletes matching entries and returns all failures.
// Entries with failed cleanup remain in the queue for a later attempt.
func (s *Storage) DeleteWhereQueued(predicate func(*Entry) bool, cleanup func(*Entry) error) error {
	entries, err := s.FilterQueued(predicate)
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if deleteQueuedErr := s.DeleteQueued(entry.InfoHash, cleanup); deleteQueuedErr != nil {
			errs = append(errs, deleteQueuedErr)
		}
	}
	return errors.Join(errs...)
}

// UpdateWhereQueued updates matching entries and returns all write failures.
func (s *Storage) UpdateWhereQueued(filter func(*Entry) bool, update func(*Entry) bool) error {
	entries, err := s.FilterQueued(filter)
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if update != nil && update(entry) {
			if updateQueueErr := s.UpdateQueue(entry); updateQueueErr != nil {
				errs = append(errs, updateQueueErr)
			}
		}
	}
	return errors.Join(errs...)
}
