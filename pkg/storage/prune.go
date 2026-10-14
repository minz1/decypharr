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
	b, err := encodeInt64(t.Unix())
	if err != nil {
		return err
	}
	return s.prune.Put(infohash, b, nil)
}

func (s *Storage) ClearUnreferencedSince(infohashes ...string) error {
	for _, h := range infohashes {
		if err := s.prune.Delete(h); err != nil && !errors.Is(err, appendstore.ErrKeyNotFound) {
			return err
		}
	}
	return nil
}

func (s *Storage) LastReferencedCount() (int, bool, error) {
	data, err := s.prune.Get(lastReferencedKey)
	if errors.Is(err, appendstore.ErrKeyNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	n, err := decodeInt64(lastReferencedKey, data)
	if err != nil {
		return 0, false, err
	}
	return int(n), true, nil
}

func (s *Storage) SetLastReferencedCount(n int) error {
	b, err := encodeInt64(int64(n))
	if err != nil {
		return err
	}
	return s.prune.Put(lastReferencedKey, b, nil)
}
