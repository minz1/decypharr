package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/sirrobot01/appendstore"
)

const lastReferencedKey = "\x00last_referenced"

const lastScanKey = "\x00last_scan"

const ledgerValueSize = 8

func encodeInt64(v int64) ([]byte, error) {
	if v < 0 {
		return nil, fmt.Errorf("prune ledger value %d is negative", v)
	}
	return binary.BigEndian.AppendUint64(nil, uint64(v)), nil
}

func decodeInt64(key string, b []byte) (int64, error) {
	if len(b) != ledgerValueSize {
		return 0, fmt.Errorf("prune ledger value for %q has %d bytes, want %d", key, len(b), ledgerValueSize)
	}
	v := binary.BigEndian.Uint64(b)
	if v > math.MaxInt64 {
		return 0, fmt.Errorf("prune ledger value for %q overflows", key)
	}
	return int64(v), nil
}

func (s *Storage) UnreferencedSince() (map[string]time.Time, error) {
	out := make(map[string]time.Time)
	err := s.prune.ForEach(func(key string, value []byte) error {
		if strings.HasPrefix(key, "\x00") {
			return nil
		}
		secs, err := decodeInt64(key, value)
		if err != nil {
			return err
		}
		out[key] = time.Unix(secs, 0)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Storage) SetUnreferencedSince(infohash string, t time.Time) error {
	return s.setInt64(infohash, t.Unix())
}

func (s *Storage) ClearUnreferencedSince(infohashes ...string) error {
	for _, h := range infohashes {
		if err := s.prune.Delete(h); err != nil && !errors.Is(err, appendstore.ErrKeyNotFound) {
			return err
		}
	}
	return nil
}

func (s *Storage) getInt64(key string) (int64, bool, error) {
	data, err := s.prune.Get(key)
	if errors.Is(err, appendstore.ErrKeyNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	v, err := decodeInt64(key, data)
	return v, err == nil, err
}

func (s *Storage) setInt64(key string, v int64) error {
	b, err := encodeInt64(v)
	if err != nil {
		return err
	}
	return s.prune.Put(key, b, nil)
}

func (s *Storage) LastReferencedCount() (int, bool, error) {
	n, ok, err := s.getInt64(lastReferencedKey)
	return int(n), ok, err
}

func (s *Storage) SetLastReferencedCount(n int) error {
	return s.setInt64(lastReferencedKey, int64(n))
}

func (s *Storage) LastScan() (time.Time, bool, error) {
	secs, ok, err := s.getInt64(lastScanKey)
	if !ok {
		return time.Time{}, false, err
	}
	return time.Unix(secs, 0), true, nil
}

func (s *Storage) SetLastScan(t time.Time) error {
	return s.setInt64(lastScanKey, t.Unix())
}
