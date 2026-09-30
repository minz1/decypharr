package storage

import (
	"errors"
	"fmt"
	"strings"
	"time"

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
	if putErr := s.queue.Put(strings.ToLower(entry.InfoHash), data, entryPutOptions(entry)); putErr != nil {
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
