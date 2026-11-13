package storage

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sirrobot01/appendstore"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// AddQueue adds an entry to the queue.
func (s *Storage) AddQueue(entry *Entry) error {
	entry.CreatedAt = time.Now()
	return s.UpdateQueue(entry)
}

// UpdateQueue updates a queued entry. It drops any grab document (Entry.Grab is not written): a writer outside
// the grab code changes the legacy fields without the document, so the document would no longer match them.
func (s *Storage) UpdateQueue(entry *Entry) error {
	return s.putQueued(entry, false)
}

// UpdateQueuedGrab updates a queued entry with its grab document (Entry.Grab). Only the grab code calls it.
func (s *Storage) UpdateQueuedGrab(entry *Entry) error {
	return s.putQueued(entry, true)
}

func (s *Storage) putQueued(entry *Entry, withGrab bool) error {
	entry.UpdatedAt = time.Now()
	data, err := encodeQueued(entry, withGrab)
	if err != nil {
		return fmt.Errorf("encode queued entry %q: %w", entry.InfoHash, err)
	}
	if putErr := s.queue.Put(strings.ToLower(entry.InfoHash), data, s.entryPutOptions(entry)); putErr != nil {
		return fmt.Errorf("save queued entry %q: %w", entry.InfoHash, putErr)
	}
	return nil
}

// GetQueued retrieves a queued entry without its grab document.
func (s *Storage) GetQueued(infohash string) (*Entry, error) {
	return s.getQueued(infohash, false)
}

// GetQueuedGrab retrieves a queued entry with its grab document (Entry.Grab). Only the grab code calls it.
func (s *Storage) GetQueuedGrab(infohash string) (*Entry, error) {
	return s.getQueued(infohash, true)
}

func (s *Storage) getQueued(infohash string, withGrab bool) (*Entry, error) {
	key := strings.ToLower(infohash)
	data, err := s.queue.Get(key)
	if err != nil {
		return nil, fmt.Errorf("read queued entry %q: %w", key, err)
	}
	entry, err := decodeQueued(data, withGrab)
	if err != nil {
		return nil, fmt.Errorf("decode queued entry %q: %w", key, err)
	}
	return entry, nil
}

// grabField carries Entry.Grab in a queue record. EntryProto does not declare it, so a binary that predates it
// decodes the record and drops the field on its next write (EntryToProto builds a fresh message).
const grabField protowire.Number = 1000

// encodeQueued encodes a queue record, with e.Grab when withGrab is set.
func encodeQueued(e *Entry, withGrab bool) ([]byte, error) {
	pb := EntryToProto(e)
	if withGrab && len(e.Grab) > 0 {
		field := protowire.AppendTag(nil, grabField, protowire.BytesType)
		pb.ProtoReflect().SetUnknown(protowire.AppendBytes(field, e.Grab))
	}
	return proto.Marshal(pb)
}

// grabDocument returns the grab field in a record's unknown fields, or nil.
func grabDocument(unknown []byte) []byte {
	var doc []byte
	for len(unknown) > 0 {
		num, typ, n := protowire.ConsumeTag(unknown)
		if n < 0 {
			return nil
		}
		unknown = unknown[n:]
		if num == grabField && typ == protowire.BytesType {
			v, vn := protowire.ConsumeBytes(unknown)
			if vn < 0 {
				return nil
			}
			doc = append([]byte(nil), v...)
			unknown = unknown[vn:]
			continue
		}
		vn := protowire.ConsumeFieldValue(num, typ, unknown)
		if vn < 0 {
			return nil
		}
		unknown = unknown[vn:]
	}
	return doc
}

// decodeQueued decodes a queue record, with its grab document when withGrab is set.
func decodeQueued(data []byte, withGrab bool) (*Entry, error) {
	var pb EntryProto
	if err := proto.Unmarshal(data, &pb); err != nil {
		return nil, err
	}
	e := ProtoToEntry(&pb)
	if withGrab {
		e.Grab = grabDocument(pb.ProtoReflect().GetUnknown())
	}
	return e, nil
}

// DeleteQueued removes an entry only after its cleanup succeeds; a missing entry is already deleted.
func (s *Storage) DeleteQueued(infohash string, cleanup func(*Entry) error) error {
	key := strings.ToLower(infohash)
	if cleanup != nil {
		entry, err := s.GetQueued(key)
		if errors.Is(err, appendstore.ErrKeyNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if cleanupErr := cleanup(entry); cleanupErr != nil {
			return fmt.Errorf("clean up queued entry %q: %w", key, cleanupErr)
		}
	}
	if err := s.queue.Delete(key); err != nil && !errors.Is(err, appendstore.ErrKeyNotFound) {
		return fmt.Errorf("delete queued entry %q: %w", key, err)
	}
	return nil
}

// FilterQueued returns matching entries. It returns an error if the scan fails.
func (s *Storage) FilterQueued(filter func(*Entry) bool) ([]*Entry, error) {
	return s.filterQueued(filter, false, func(key string, err error) error {
		return fmt.Errorf("decode queued entry %q: %w", key, err)
	})
}

// FilterQueuedGrab is FilterQueued with each record's grab document (Entry.Grab). Only the grab code calls it.
// A record that does not decode is skipped: it returns every record it read, with the decode errors joined.
func (s *Storage) FilterQueuedGrab(filter func(*Entry) bool) ([]*Entry, error) {
	var bad []error
	entries, err := s.filterQueued(filter, true, func(key string, err error) error {
		bad = append(bad, fmt.Errorf("decode queued entry %q: %w", key, err))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, errors.Join(bad...)
}

func (s *Storage) filterQueued(filter func(*Entry) bool, withGrab bool, undecoded func(key string, err error) error) (
	[]*Entry,
	error,
) {
	var entries []*Entry
	err := s.queue.ForEach(func(key string, value []byte) error {
		entry, err := decodeQueued(value, withGrab)
		if err != nil {
			return undecoded(key, err)
		}
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
