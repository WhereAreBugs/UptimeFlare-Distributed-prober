package agent

import (
	"runtime"
)

// localStatus contains only operational counters; it is never uploaded.
type localStatus struct {
	CheckingPaused bool   `json:"checking_paused"`
	Version        int    `json:"version"`
	StartedAt      int64  `json:"started_at"`
	Build          string `json:"build"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	QueueCount     uint64 `json:"queue_count"`
	QueueBytes     int64  `json:"queue_bytes"`
	DatabaseBytes  int64  `json:"database_bytes"`
	DatabaseLimit  int64  `json:"database_limit"`
	OldestQueuedAt int64  `json:"oldest_queued_at"`
	MonitorCount   int    `json:"monitor_count"`
	LastConfigAt   int64  `json:"last_config_at"`
	LastUploadAt   int64  `json:"last_upload_at"`
	UploadState    string `json:"upload_state"`
	NextUploadAt   int64  `json:"next_upload_at"`
}

func (a *Agent) status() (localStatus, error) {
	queue, err := a.queue.Snapshot()
	if err != nil {
		return localStatus{}, err
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	build := a.opts.Version
	if build == "" {
		build = "dev"
	}
	return localStatus{CheckingPaused: a.checkingPaused, Version: 1, StartedAt: a.startedAt.Unix(), Build: build, OS: runtime.GOOS, Arch: runtime.GOARCH,
		QueueCount: queue.Count, QueueBytes: queue.Bytes, DatabaseBytes: queue.DatabaseBytes, DatabaseLimit: queue.DatabaseLimit, OldestQueuedAt: queue.OldestAt,
		MonitorCount: len(a.config.Monitors), LastConfigAt: a.lastConfigAt, LastUploadAt: max(a.lastUploadAt, queue.LastAckAt), UploadState: a.uploadState, NextUploadAt: a.nextUploadAt}, nil
}
