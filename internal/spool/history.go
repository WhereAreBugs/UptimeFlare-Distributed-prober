package spool

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	bolt "go.etcd.io/bbolt"
	"light-prober/internal/protocol"
	"os"
	"time"
)

var historyKey = []byte("history-v1")
var dailyHistoryKey = []byte("history-daily-v1")
var latestKey = []byte("latest")

const HistorySeconds int64 = 12 * 60 * 60
const DailyHistoryDays int64 = 90
const daySeconds int64 = 24 * 60 * 60

// Existing pending samples seed the new rolling history once. Uploaded samples
// that an older binary already removed cannot be reconstructed locally.
func (q *Queue) initializeHistory() error {
	return q.update(func(tx *bolt.Tx) error {
		if tx.Bucket(historyKey) != nil {
			return nil
		}
		if _, err := tx.CreateBucket(historyKey); err != nil {
			return err
		}
		// Never put a legacy outbox under pressure just to build optional history.
		if tx.Size()+(32<<20) > q.maxDatabaseBytes {
			return nil
		}
		now := time.Now().Unix()
		latest := make(map[string]protocol.Result)
		err := tx.Bucket(resultsKey).ForEach(func(_, data []byte) error {
			result, err := decodeResult(data)
			if err != nil {
				return err
			}
			if _, ok := latest[result.MonitorID]; !ok && len(latest) >= protocol.MaxMonitors {
				return nil
			}
			if result.Time > latest[result.MonitorID].Time {
				latest[result.MonitorID] = protocol.Result{MonitorID: result.MonitorID, Time: result.Time, Up: result.Up, LatencyMS: result.LatencyMS, Stage: result.Stage, Code: result.Code}
			}
			if result.Time >= now-HistorySeconds && result.Time <= now {
				return appendRecentHistory(tx, result)
			}
			return nil
		})
		if err != nil {
			return err
		}
		root := tx.Bucket(historyKey)
		for id, result := range latest {
			bucket := root.Bucket([]byte(id))
			if bucket != nil && bucket.Get(latestKey) != nil {
				continue
			}
			if bucket == nil {
				var err error
				bucket, err = root.CreateBucket([]byte(id))
				if err != nil {
					return err
				}
			}
			result.MonitorID = ""
			result.Stage = result.Stage[:min(len(result.Stage), 32)]
			result.Code = result.Code[:min(len(result.Code), 128)]
			data, err := json.Marshal(result)
			if err != nil {
				return err
			}
			if err = bucket.Put(latestKey, data); err != nil {
				return err
			}
		}
		return nil
	})
}

type HistoryBucket struct {
	Time          int64   `json:"time"`
	Checks        uint64  `json:"checks"`
	Failures      uint64  `json:"failures"`
	LatencySum    float64 `json:"latency_sum"`
	LatencyChecks uint64  `json:"latency_checks"`
}
type History struct {
	Latest       *protocol.Result `json:"latest"`
	Buckets      []HistoryBucket  `json:"buckets"`
	DailyBuckets []HistoryBucket  `json:"daily_buckets"`
}
type Snapshot struct {
	Count                                                    uint64
	Bytes, DatabaseBytes, DatabaseLimit, OldestAt, LastAckAt int64
}

// Called in the SAME durable transaction as the outbox append. ACK never
// touches this rolling history; it survives uploads and process restarts.
func appendHistory(tx *bolt.Tx, result protocol.Result) error {
	if err := appendRecentHistory(tx, result); err != nil {
		return err
	}
	return addDailyHistory(tx, result.MonitorID, sampleBucket(result, daySeconds))
}

func appendRecentHistory(tx *bolt.Tx, result protocol.Result) error {
	root, err := tx.CreateBucketIfNotExists(historyKey)
	if err != nil {
		return err
	}
	bucket := root.Bucket([]byte(result.MonitorID))
	if bucket == nil {
		count := 0
		if err := root.ForEach(func(_, value []byte) error {
			if value == nil {
				count++
			}
			return nil
		}); err != nil {
			return err
		}
		if count >= protocol.MaxMonitors {
			return nil
		}
		bucket, err = root.CreateBucket([]byte(result.MonitorID))
		if err != nil {
			return err
		}
	}
	var latest protocol.Result
	if data := bucket.Get(latestKey); data != nil {
		if json.Unmarshal(data, &latest) != nil {
			return ErrCorrupt
		}
	}
	if result.Time >= latest.Time {
		safe := protocol.Result{Time: result.Time, Up: result.Up, LatencyMS: result.LatencyMS, Stage: result.Stage, Code: result.Code}
		if len(safe.Stage) > 32 {
			safe.Stage = "unknown"
		}
		if len(safe.Code) > 128 {
			safe.Code = "unknown"
		}
		data, err := json.Marshal(safe)
		if err != nil {
			return err
		}
		if err = bucket.Put(latestKey, data); err != nil {
			return err
		}
	}
	time := result.Time / 300 * 300
	cutoff := max(result.Time, latest.Time)/300*300 - HistorySeconds
	cursor := bucket.Cursor()
	for key, _ := cursor.First(); key != nil && len(key) == 8 && int64(binary.BigEndian.Uint64(key)) < cutoff; key, _ = cursor.First() {
		if err := cursor.Delete(); err != nil {
			return err
		}
	}
	if time < cutoff {
		return nil
	}
	key := encode(uint64(time))
	value := HistoryBucket{Time: time}
	if data := bucket.Get(key); data != nil {
		value, err = decodeHistoryBucket(data)
		if err != nil {
			return err
		}
	}
	value.Checks++
	if !result.Up {
		value.Failures++
	} else {
		value.LatencySum += result.LatencyMS
		value.LatencyChecks++
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return bucket.Put(key, data)
}

func sampleBucket(result protocol.Result, seconds int64) HistoryBucket {
	value := HistoryBucket{Time: result.Time / seconds * seconds, Checks: 1}
	if result.Up {
		value.LatencySum, value.LatencyChecks = result.LatencyMS, 1
	} else {
		value.Failures = 1
	}
	return value
}

// Legacy five-minute buckets included failure duration in latency_sum. Only
// wholly successful legacy buckets have a recoverable successful average.
func decodeHistoryBucket(data []byte) (HistoryBucket, error) {
	var wire struct {
		Time          int64   `json:"time"`
		Checks        uint64  `json:"checks"`
		Failures      uint64  `json:"failures"`
		LatencySum    float64 `json:"latency_sum"`
		LatencyChecks *uint64 `json:"latency_checks"`
	}
	if json.Unmarshal(data, &wire) != nil || wire.Failures > wire.Checks || wire.LatencySum < 0 {
		return HistoryBucket{}, ErrCorrupt
	}
	value := HistoryBucket{Time: wire.Time, Checks: wire.Checks, Failures: wire.Failures, LatencySum: wire.LatencySum}
	if wire.LatencyChecks != nil {
		value.LatencyChecks = *wire.LatencyChecks
	} else if wire.Failures == 0 {
		value.LatencyChecks = wire.Checks
	} else {
		value.LatencySum = 0
	}
	if value.LatencyChecks > value.Checks-value.Failures {
		return HistoryBucket{}, ErrCorrupt
	}
	return value, nil
}

func readHistoryBuckets(bucket *bolt.Bucket, cutoff, now int64, limit int) ([]HistoryBucket, error) {
	values := []HistoryBucket{}
	cursor := bucket.Cursor()
	for key, data := cursor.Seek(encode(uint64(max(0, cutoff)))); key != nil && len(key) == 8; key, data = cursor.Next() {
		if int64(binary.BigEndian.Uint64(key)) > now {
			break
		}
		value, err := decodeHistoryBucket(data)
		if err != nil || value.Time != int64(binary.BigEndian.Uint64(key)) {
			return nil, ErrCorrupt
		}
		values = append(values, value)
		if len(values) > limit {
			return nil, errors.New("history window exceeds limit")
		}
	}
	return values, nil
}

// Daily aggregates keep 90 UTC days, rather than 90 days of raw samples. The
// same append transaction commits both windows and the unchanged upload queue.
func addDailyHistory(tx *bolt.Tx, monitorID string, addition HistoryBucket) error {
	root, err := tx.CreateBucketIfNotExists(dailyHistoryKey)
	if err != nil {
		return err
	}
	bucket := root.Bucket([]byte(monitorID))
	if bucket == nil {
		count := 0
		if err := root.ForEach(func(_, data []byte) error {
			if data == nil {
				count++
			}
			return nil
		}); err != nil {
			return err
		}
		if count >= protocol.MaxMonitors {
			return nil
		}
		bucket, err = root.CreateBucket([]byte(monitorID))
		if err != nil {
			return err
		}
	}
	anchor := addition.Time
	if last, _ := bucket.Cursor().Last(); len(last) == 8 {
		anchor = max(anchor, int64(binary.BigEndian.Uint64(last)))
	}
	cutoff := anchor - (DailyHistoryDays-1)*daySeconds
	cursor := bucket.Cursor()
	for key, _ := cursor.First(); key != nil; key, _ = cursor.First() {
		if len(key) != 8 {
			return ErrCorrupt
		}
		if int64(binary.BigEndian.Uint64(key)) >= cutoff {
			break
		}
		if err := cursor.Delete(); err != nil {
			return err
		}
	}
	if addition.Time < cutoff {
		return nil
	}
	key := encode(uint64(addition.Time))
	value := HistoryBucket{Time: addition.Time}
	if data := bucket.Get(key); data != nil {
		value, err = decodeHistoryBucket(data)
		if err != nil {
			return err
		}
	}
	checks := value.Checks + addition.Checks
	if checks < value.Checks {
		return ErrCorrupt
	}
	value.Checks = checks
	value.Failures += addition.Failures
	value.LatencySum += addition.LatencySum
	value.LatencyChecks += addition.LatencyChecks
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return bucket.Put(key, data)
}

// Upgrade once, without altering outbox keys, values or counters. Seed retained
// five-minute history first; pending samples already covered by those buckets
// must not be counted a second time. Older pending samples recover older days.
func (q *Queue) initializeDailyHistory() error {
	return q.update(func(tx *bolt.Tx) error {
		if tx.Bucket(dailyHistoryKey) != nil {
			return nil
		}
		if _, err := tx.CreateBucket(dailyHistoryKey); err != nil {
			return err
		}
		// Optional backfill must never endanger a nearly full real upload queue.
		if tx.Size()+(32<<20) > q.maxDatabaseBytes {
			return nil
		}
		now := time.Now().Unix()
		cutoff := now/daySeconds*daySeconds - (DailyHistoryDays-1)*daySeconds
		recent := tx.Bucket(historyKey)
		if recent != nil {
			if err := recent.ForEach(func(id, data []byte) error {
				if data != nil {
					return ErrCorrupt
				}
				buckets, err := readHistoryBuckets(recent.Bucket(id), cutoff, now, 145)
				if err != nil {
					return err
				}
				for _, value := range buckets {
					value.Time = value.Time / daySeconds * daySeconds
					if err := addDailyHistory(tx, string(id), value); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return err
			}
		}
		return tx.Bucket(resultsKey).ForEach(func(_, data []byte) error {
			result, err := decodeResult(data)
			if err != nil {
				return err
			}
			if result.Time < cutoff || result.Time > now {
				return nil
			}
			if recent != nil {
				if bucket := recent.Bucket([]byte(result.MonitorID)); bucket != nil && bucket.Get(encode(uint64(result.Time/300*300))) != nil {
					return nil
				}
			}
			return addDailyHistory(tx, result.MonitorID, sampleBucket(result, daySeconds))
		})
	})
}

func (q *Queue) Snapshot() (value Snapshot, err error) {
	err = q.view(func(tx *bolt.Tx) error {
		value.Count, value.Bytes, err = stats(tx)
		if ack := tx.Bucket(metaKey).Get([]byte("last-ack-at")); len(ack) == 8 {
			value.LastAckAt = int64(binary.BigEndian.Uint64(ack))
		}
		if err != nil {
			return err
		}
		_, data := tx.Bucket(resultsKey).Cursor().First()
		if data != nil {
			result, e := decodeResult(data)
			if e != nil {
				return e
			}
			value.OldestAt = result.Time
		}
		info, e := os.Stat(q.db.Path())
		if e != nil {
			return e
		}
		value.DatabaseBytes = info.Size()
		value.DatabaseLimit = q.maxDatabaseBytes
		return nil
	})
	return
}
func (q *Queue) History(monitorID string, now int64) (value History, err error) {
	value.Buckets = []HistoryBucket{}
	value.DailyBuckets = []HistoryBucket{}
	err = q.view(func(tx *bolt.Tx) error {
		if root := tx.Bucket(dailyHistoryKey); root != nil {
			if bucket := root.Bucket([]byte(monitorID)); bucket != nil {
				cutoff := now/daySeconds*daySeconds - (DailyHistoryDays-1)*daySeconds
				var err error
				value.DailyBuckets, err = readHistoryBuckets(bucket, cutoff, now, int(DailyHistoryDays))
				if err != nil {
					return err
				}
			}
		}
		root := tx.Bucket(historyKey)
		if root == nil {
			return nil
		}
		bucket := root.Bucket([]byte(monitorID))
		if bucket == nil {
			return nil
		}
		if data := bucket.Get(latestKey); data != nil {
			value.Latest = new(protocol.Result)
			if json.Unmarshal(data, value.Latest) != nil {
				return ErrCorrupt
			}
		}
		cutoff := now/300*300 - HistorySeconds
		var err error
		value.Buckets, err = readHistoryBuckets(bucket, cutoff, now, 145)
		return err
	})
	return
}

func (q *Queue) Latest(monitorID string) (value *protocol.Result, err error) {
	err = q.view(func(tx *bolt.Tx) error {
		root := tx.Bucket(historyKey)
		if root == nil {
			return nil
		}
		bucket := root.Bucket([]byte(monitorID))
		if bucket == nil {
			return nil
		}
		data := bucket.Get(latestKey)
		if data == nil {
			return nil
		}
		value = new(protocol.Result)
		if json.Unmarshal(data, value) != nil {
			return ErrCorrupt
		}
		return nil
	})
	return
}

// Removed assignments cannot accumulate unbounded historical target buckets.
func (q *Queue) PruneHistory(ids []string) error {
	keep := make(map[string]bool, len(ids))
	for _, id := range ids {
		keep[id] = true
	}
	return q.update(func(tx *bolt.Tx) error {
		for _, rootKey := range [][]byte{historyKey, dailyHistoryKey} {
			root := tx.Bucket(rootKey)
			if root == nil {
				continue
			}
			var removed [][]byte
			if err := root.ForEach(func(key, value []byte) error {
				if value == nil && !keep[string(key)] {
					removed = append(removed, append([]byte(nil), key...))
				}
				return nil
			}); err != nil {
				return err
			}
			for _, key := range removed {
				if err := root.DeleteBucket(key); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
