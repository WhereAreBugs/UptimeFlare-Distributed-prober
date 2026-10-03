// Package spool implements a synchronous, persistent FIFO outbox. A committed
// result remains queued until the server explicitly acknowledges that result.
package spool

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"light-prober/internal/protocol"

	bolt "go.etcd.io/bbolt"
)

const (
	maxResultBytes = 64 << 10
	recordHeader   = 5 // format version plus CRC32C of JSON payload
)

var checksumTable = crc32.MakeTable(crc32.Castagnoli)

var (
	ErrFull         = errors.New("persistent queue is full")
	ErrTooLarge     = errors.New("result exceeds persistent queue entry limit")
	ErrCorrupt      = errors.New("persistent queue is corrupt")
	ErrMismatch     = errors.New("acknowledged result does not match queued result")
	ErrLocked       = errors.New("persistent queue is already open by another process")
	ErrClosed       = errors.New("persistent queue is closed")
	resultsKey      = []byte("results-v1")
	metaKey         = []byte("meta-v1")
	countKey        = []byte("count")
	bytesKey        = []byte("bytes")
	lastTimeKey     = []byte("last-time")
	monitorTimesKey = []byte("monitor-times-v1")
	monitorFloorKey = []byte("monitor-time-floor")
)

type Entry struct {
	ID     uint64
	Result protocol.Result
}

type Queue struct {
	db       *bolt.DB
	maxBytes int64
	mu       sync.RWMutex
	closed   bool
}

// Open takes an exclusive file lock. maxBytes limits serialized result payload
// bytes; bbolt pages and filesystem overhead are not part of this limit.
func Open(path string, maxBytes int64) (*Queue, error) {
	if maxBytes <= 0 {
		return nil, errors.New("queue byte limit must be positive")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create queue directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("protect queue directory: %w", err)
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("queue path must be a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect queue file: %w", err)
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 150 * time.Millisecond})
	if errors.Is(err, bolt.ErrTimeout) {
		return nil, ErrLocked
	}
	if err != nil {
		return nil, fmt.Errorf("open queue: %w", err)
	}
	q := &Queue{db: db, maxBytes: maxBytes}
	if err = os.Chmod(path, 0600); err == nil {
		err = db.Update(func(tx *bolt.Tx) error {
			if err := initialize(tx); err != nil {
				return err
			}
			return initializeMonitorTimes(tx)
		})
	}
	if err == nil {
		err = db.View(verify)
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return q, nil
}

func initializeMonitorTimes(tx *bolt.Tx) error {
	if tx.Bucket(monitorTimesKey) != nil {
		return nil
	}
	times, err := tx.CreateBucket(monitorTimesKey)
	if err != nil {
		return err
	}
	lastTime, err := readLastTime(tx)
	if err != nil {
		return err
	}
	if err := tx.Bucket(metaKey).Put(monitorFloorKey, encode(uint64(lastTime))); err != nil {
		return err
	}
	return tx.Bucket(resultsKey).ForEach(func(_, value []byte) error {
		result, err := decodeResult(value)
		if err != nil {
			return err
		}
		key := monitorTimeKey(result.MonitorID)
		previous := times.Get(key)
		if previous == nil || result.Time > int64(binary.BigEndian.Uint64(previous)) {
			return times.Put(key, encode(uint64(max(int64(0), result.Time))))
		}
		return nil
	})
}

func initialize(tx *bolt.Tx) error {
	if tx.Bucket(resultsKey) != nil || tx.Bucket(metaKey) != nil {
		if tx.Bucket(resultsKey) == nil || tx.Bucket(metaKey) == nil {
			return ErrCorrupt
		}
		if tx.Bucket(metaKey).Get(lastTimeKey) == nil {
			// Upgrade existing queues by recovering the largest queued timestamp.
			// The cursor is retained permanently once created, including after ACK.
			var lastTime int64
			if err := tx.Bucket(resultsKey).ForEach(func(_, value []byte) error {
				result, err := decodeResult(value)
				if err != nil {
					return err
				}
				lastTime = max(lastTime, result.Time)
				return nil
			}); err != nil {
				return err
			}
			return tx.Bucket(metaKey).Put(lastTimeKey, encode(uint64(lastTime)))
		}
		return nil
	}
	if _, err := tx.CreateBucket(resultsKey); err != nil {
		return err
	}
	meta, err := tx.CreateBucket(metaKey)
	if err != nil {
		return err
	}
	if err := meta.Put(countKey, encode(0)); err != nil {
		return err
	}
	if err := meta.Put(bytesKey, encode(0)); err != nil {
		return err
	}
	return meta.Put(lastTimeKey, encode(0))
}

func verify(tx *bolt.Tx) error {
	expectedCount, expectedBytes, err := stats(tx)
	if err != nil {
		return err
	}
	var count, payloadBytes uint64
	var maxTime int64
	entries := tx.Bucket(resultsKey)
	err = entries.ForEach(func(key, value []byte) error {
		if len(key) != 8 || binary.BigEndian.Uint64(key) == 0 || value == nil {
			return ErrCorrupt
		}
		result, err := decodeResult(value)
		if err != nil {
			return err
		}
		maxTime = max(maxTime, result.Time)
		monitorTime, err := readMonitorTime(tx, result.MonitorID)
		if err != nil || monitorTime < result.Time {
			return ErrCorrupt
		}
		count++
		payloadBytes += uint64(len(value) - recordHeader)
		return nil
	})
	if err != nil {
		return err
	}
	if expectedCount != count || expectedBytes != int64(payloadBytes) {
		return ErrCorrupt
	}
	lastTime, err := readLastTime(tx)
	if err != nil || lastTime < maxTime {
		return ErrCorrupt
	}
	if tx.Bucket(monitorTimesKey) == nil {
		return ErrCorrupt
	}
	floor := tx.Bucket(metaKey).Get(monitorFloorKey)
	if len(floor) != 8 || binary.BigEndian.Uint64(floor) > uint64(lastTime) {
		return ErrCorrupt
	}
	if err := tx.Bucket(monitorTimesKey).ForEach(func(key, value []byte) error {
		if len(key) == 0 || key[0] != 1 || len(value) != 8 || binary.BigEndian.Uint64(value) > uint64(lastTime) {
			return ErrCorrupt
		}
		return nil
	}); err != nil {
		return err
	}
	last, _ := entries.Cursor().Last()
	if last != nil && entries.Sequence() < binary.BigEndian.Uint64(last) {
		return ErrCorrupt
	}
	return nil
}

func (q *Queue) Append(result protocol.Result) error {
	data, err := marshalResult(result)
	if err != nil {
		return err
	}
	return q.update(func(tx *bolt.Tx) error {
		count, used, err := stats(tx)
		if err != nil {
			return err
		}
		if int64(len(data)) > q.maxBytes-used {
			return ErrFull
		}
		entries := tx.Bucket(resultsKey)
		id, err := entries.NextSequence()
		if err != nil {
			return err
		}
		if id == 0 || count == math.MaxUint64 {
			return errors.New("persistent queue sequence exhausted")
		}
		if err := entries.Put(encode(id), encodeRecord(data)); err != nil {
			return err
		}
		lastTime, err := readLastTime(tx)
		if err != nil {
			return err
		}
		if result.Time > lastTime {
			if err := tx.Bucket(metaKey).Put(lastTimeKey, encode(uint64(result.Time))); err != nil {
				return err
			}
		}
		monitorTime, err := readMonitorTime(tx, result.MonitorID)
		if err != nil {
			return err
		}
		if result.Time > monitorTime || tx.Bucket(monitorTimesKey).Get(monitorTimeKey(result.MonitorID)) == nil {
			if err := tx.Bucket(monitorTimesKey).Put(monitorTimeKey(result.MonitorID), encode(uint64(max(int64(0), result.Time)))); err != nil {
				return err
			}
		}
		return putStats(tx, count+1, used+int64(len(data)))
	})
}

// Peek returns at most limit entries without removing them. No partial result
// is returned if any entry in the requested range fails validation.
func (q *Queue) Peek(limit int) ([]Entry, error) {
	if limit < 1 || limit > protocol.MaxBatchResults {
		return nil, fmt.Errorf("queue peek limit must be between 1 and %d", protocol.MaxBatchResults)
	}
	var result []Entry
	err := q.view(func(tx *bolt.Tx) error {
		if _, _, err := stats(tx); err != nil {
			return err
		}
		cursor := tx.Bucket(resultsKey).Cursor()
		for key, value := cursor.First(); key != nil && len(result) < limit; key, value = cursor.Next() {
			if len(key) != 8 || binary.BigEndian.Uint64(key) == 0 {
				return ErrCorrupt
			}
			item, err := decodeResult(value)
			if err != nil {
				return err
			}
			result = append(result, Entry{ID: binary.BigEndian.Uint64(key), Result: item})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Ack deletes only entries whose ID and serialized content both match. Missing
// IDs are harmless, making an already committed acknowledgment idempotent.
func (q *Queue) Ack(entries []Entry) error {
	if len(entries) > protocol.MaxBatchResults {
		return fmt.Errorf("queue acknowledgment exceeds %d entries", protocol.MaxBatchResults)
	}
	return q.update(func(tx *bolt.Tx) error {
		count, used, err := stats(tx)
		if err != nil {
			return err
		}
		bucket := tx.Bucket(resultsKey)
		for _, entry := range entries {
			key := encode(entry.ID)
			value := bucket.Get(key)
			if value == nil {
				continue
			}
			data, err := marshalResult(entry.Result)
			if err != nil {
				return err
			}
			if _, err := decodeResult(value); err != nil {
				return err
			}
			if !bytes.Equal(value[recordHeader:], data) {
				return ErrMismatch
			}
			if count == 0 || used < int64(len(value)-recordHeader) {
				return ErrCorrupt
			}
			count--
			used -= int64(len(value) - recordHeader)
			if err := bucket.Delete(key); err != nil {
				return err
			}
		}
		return putStats(tx, count, used)
	})
}

func (q *Queue) Stats() (count uint64, payloadBytes int64, err error) {
	err = q.view(func(tx *bolt.Tx) error {
		var readErr error
		count, payloadBytes, readErr = stats(tx)
		return readErr
	})
	return
}

// LastTime returns the greatest timestamp ever durably appended. Acknowledging
// or emptying the queue does not reset it, preventing sample identity reuse
// when a process restarts within the same wall-clock second.
func (q *Queue) LastTime() (lastTime int64, err error) {
	err = q.view(func(tx *bolt.Tx) error {
		lastTime, err = readLastTime(tx)
		return err
	})
	return
}

// LastTimeFor keeps independent target clocks while preserving identity across
// restarts and configuration removal/re-addition. Unknown targets conservatively
// use the migration-time cursor for an older acknowledged, empty queue.
func (q *Queue) LastTimeFor(monitorID string) (lastTime int64, err error) {
	err = q.view(func(tx *bolt.Tx) error {
		lastTime, err = readMonitorTime(tx, monitorID)
		return err
	})
	return
}

func monitorTimeKey(id string) []byte {
	// A prefix also represents the empty ID without violating bbolt's key limit.
	return append([]byte{1}, id...)
}

func readMonitorTime(tx *bolt.Tx, monitorID string) (int64, error) {
	times := tx.Bucket(monitorTimesKey)
	if times == nil {
		return 0, ErrCorrupt
	}
	value := times.Get(monitorTimeKey(monitorID))
	if value == nil {
		value = tx.Bucket(metaKey).Get(monitorFloorKey)
	}
	if len(value) != 8 || binary.BigEndian.Uint64(value) > math.MaxInt64 {
		return 0, ErrCorrupt
	}
	return int64(binary.BigEndian.Uint64(value)), nil
}

func (q *Queue) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil
	}
	q.closed = true
	return q.db.Close()
}

func (q *Queue) view(fn func(*bolt.Tx) error) error {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		return ErrClosed
	}
	return q.db.View(fn)
}

func (q *Queue) update(fn func(*bolt.Tx) error) error {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		return ErrClosed
	}
	return q.db.Update(fn)
}

func stats(tx *bolt.Tx) (uint64, int64, error) {
	meta := tx.Bucket(metaKey)
	if meta == nil || tx.Bucket(resultsKey) == nil {
		return 0, 0, ErrCorrupt
	}
	count, size := meta.Get(countKey), meta.Get(bytesKey)
	if len(count) != 8 || len(size) != 8 || binary.BigEndian.Uint64(size) > math.MaxInt64 {
		return 0, 0, ErrCorrupt
	}
	if _, err := readLastTime(tx); err != nil {
		return 0, 0, err
	}
	return binary.BigEndian.Uint64(count), int64(binary.BigEndian.Uint64(size)), nil
}

func readLastTime(tx *bolt.Tx) (int64, error) {
	meta := tx.Bucket(metaKey)
	if meta == nil {
		return 0, ErrCorrupt
	}
	value := meta.Get(lastTimeKey)
	if len(value) != 8 || binary.BigEndian.Uint64(value) > math.MaxInt64 {
		return 0, ErrCorrupt
	}
	return int64(binary.BigEndian.Uint64(value)), nil
}

func putStats(tx *bolt.Tx, count uint64, payloadBytes int64) error {
	meta := tx.Bucket(metaKey)
	if err := meta.Put(countKey, encode(count)); err != nil {
		return err
	}
	return meta.Put(bytesKey, encode(uint64(payloadBytes)))
}

func encode(value uint64) []byte {
	var buffer [8]byte
	binary.BigEndian.PutUint64(buffer[:], value)
	return buffer[:]
}

func marshalResult(result protocol.Result) ([]byte, error) {
	if len(result.MonitorID)+len(result.Stage)+len(result.Code)+len(result.Message) > maxResultBytes {
		return nil, ErrTooLarge
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("serialize queue result: %w", err)
	}
	if len(data) > maxResultBytes {
		return nil, ErrTooLarge
	}
	return data, nil
}

func decodeResult(data []byte) (protocol.Result, error) {
	var result protocol.Result
	if len(data) <= recordHeader || len(data) > maxResultBytes+recordHeader || data[0] != 1 {
		return result, ErrCorrupt
	}
	payload := data[recordHeader:]
	if binary.BigEndian.Uint32(data[1:recordHeader]) != crc32.Checksum(payload, checksumTable) || json.Unmarshal(payload, &result) != nil {
		return result, ErrCorrupt
	}
	return result, nil
}

func encodeRecord(payload []byte) []byte {
	record := make([]byte, recordHeader+len(payload))
	record[0] = 1
	binary.BigEndian.PutUint32(record[1:recordHeader], crc32.Checksum(payload, checksumTable))
	copy(record[recordHeader:], payload)
	return record
}
