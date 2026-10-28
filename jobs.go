package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type State string

const (
	StateQueued      State = "queued"
	StateDownloading State = "downloading" // the NVR is in use; conversion already runs alongside
	StateTranscoding State = "transcoding" // download finished, conversion still running
	StateReady       State = "ready"
	StateError       State = "error"
	StateCancelled   State = "cancelled"
)

func (s State) active() bool {
	return s == StateQueued || s == StateDownloading || s == StateTranscoding
}

// Job is both the internal record and, via view(), the API representation.
type Job struct {
	ID              string     `json:"id"`
	Channel         int        `json:"channel"`
	Start           int64      `json:"start"`
	Duration        int        `json:"duration"`
	State           State      `json:"state"`
	Message         string     `json:"message,omitempty"`
	Created         time.Time  `json:"created"`
	Finished        *time.Time `json:"finished,omitempty"`
	DownloadSeconds float64    `json:"download_seconds,omitempty"`
	TotalSeconds    float64    `json:"total_seconds,omitempty"`
	RawMiB          float64    `json:"raw_mib,omitempty"`
	Size            int64      `json:"size,omitempty"`
	VideoDuration   float64    `json:"video_duration,omitempty"`

	cancel    context.CancelFunc
	cancelled bool
	video     string
}

func (j *Job) view() Job {
	c := *j
	c.cancel = nil
	c.video = ""
	return c
}

var (
	errQueueFull    = errors.New("the transcoder queue is full; try again in a few minutes")
	errNotFound     = errors.New("job not found")
	errShuttingDown = errors.New("the transcoder is shutting down")
)

type notYetError struct{ availableAt int64 }

func (e *notYetError) Error() string { return "the interval has not finished recording yet" }

type invalidError struct{ msg string }

func (e *invalidError) Error() string { return e.msg }

// Manager owns the job table and runs jobs strictly one at a time.
type Manager struct {
	cfg   *Config
	nvr   *digestClient
	now   func() time.Time
	queue chan *Job
	done  chan struct{}

	mu     sync.Mutex
	jobs   map[string]*Job
	closed bool
}

func NewManager(cfg *Config) (*Manager, error) {
	if err := os.MkdirAll(cfg.WorkDir, 0o700); err != nil {
		return nil, err
	}
	if err := cleanWorkDir(cfg.WorkDir); err != nil {
		return nil, err
	}
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		ResponseHeaderTimeout: 20 * time.Second,
		MaxIdleConns:          2,
		IdleConnTimeout:       30 * time.Second,
	}
	return &Manager{
		cfg:   cfg,
		nvr:   &digestClient{client: &http.Client{Transport: transport}, username: cfg.NVR.Username, password: cfg.NVR.Password},
		now:   time.Now,
		queue: make(chan *Job, cfg.Limits.MaxQueue),
		done:  make(chan struct{}),
		jobs:  map[string]*Job{},
	}, nil
}

// cleanWorkDir removes leftovers of a previous run. Only our own file types,
// only directly inside the work dir, never through a symlink.
func cleanWorkDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if n := e.Name(); strings.HasSuffix(n, ".dav") || strings.HasSuffix(n, ".mp4") {
			_ = os.Remove(filepath.Join(dir, n))
		}
	}
	return nil
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failing means the system is unusable anyway
	}
	return hex.EncodeToString(b)
}

// Submit validates a request and queues it, or returns the existing job for
// the same interval so a caller that polls or retries never doubles the work.
func (m *Manager) Submit(channel int, start int64, duration int) (Job, error) {
	l := m.cfg.Limits
	switch {
	case channel < 1 || channel > m.cfg.NVR.MaxChannel:
		return Job{}, &invalidError{"channel out of range"}
	case duration < 1 || duration > l.MaxDuration:
		return Job{}, &invalidError{"duration out of range"}
	case start < 946684800:
		return Job{}, &invalidError{"start is not a plausible Unix time"}
	}
	availableAt := start + int64(duration) + int64(l.SettleSeconds)
	if m.now().Unix() < availableAt {
		return Job{}, &notYetError{availableAt}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Job{}, errShuttingDown
	}
	active := 0
	for _, j := range m.jobs {
		if j.Channel == channel && j.Start == start && j.Duration == duration &&
			!j.cancelled && (j.State.active() || j.State == StateReady) {
			return j.view(), nil
		}
		if j.State.active() {
			active++
		}
	}
	if active >= l.MaxQueue {
		return Job{}, errQueueFull
	}
	j := &Job{ID: newID(), Channel: channel, Start: start, Duration: duration, State: StateQueued, Created: m.now()}
	select {
	case m.queue <- j:
	default:
		return Job{}, errQueueFull
	}
	m.jobs[j.ID] = j
	return j.view(), nil
}

func (m *Manager) Get(id string) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return Job{}, errNotFound
	}
	return j.view(), nil
}

// Video returns the finished file of a ready job.
func (m *Manager) Video(id string) (string, Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return "", Job{}, errNotFound
	}
	return j.video, j.view(), nil
}

// Delete cancels an unfinished job (dropping the NVR connection and killing
// ffmpeg) or frees a finished one. The caller deletes a job once it has
// fetched the video, and to stop a download when an alert needs the NVR.
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return errNotFound
	}
	switch {
	case j.State == StateQueued:
		j.cancelled = true
		j.State = StateCancelled
		delete(m.jobs, id)
	case j.State.active():
		j.cancelled = true
		if j.cancel != nil {
			j.cancel()
		}
	default:
		if j.video != "" {
			_ = os.Remove(j.video)
		}
		delete(m.jobs, id)
	}
	return nil
}

func (m *Manager) Status() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	active := 0
	for _, j := range m.jobs {
		if j.State.active() {
			active++
		}
	}
	return map[string]any{"version": version, "hwaccel": m.cfg.HWAccel, "active": active, "jobs": len(m.jobs)}
}

func (m *Manager) setState(j *Job, s State) {
	m.mu.Lock()
	if !j.cancelled {
		j.State = s
	}
	m.mu.Unlock()
}

// Run processes the queue until ctx ends, and expires old results.
func (m *Manager) Run(ctx context.Context) {
	defer close(m.done)
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			m.mu.Lock()
			m.closed = true
			m.mu.Unlock()
			return
		case <-tick.C:
			m.expire()
		case j := <-m.queue:
			m.runJob(ctx, j)
		}
	}
}

// Wait blocks until Run has returned.
func (m *Manager) Wait() { <-m.done }

func (m *Manager) runJob(parent context.Context, j *Job) {
	m.mu.Lock()
	if j.cancelled {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(m.cfg.Limits.JobTimeout)*time.Second)
	defer cancel()
	j.cancel = cancel
	j.State = StateDownloading
	m.mu.Unlock()

	res, err := m.process(ctx, j)

	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	j.Finished = &now
	j.cancel = nil
	switch {
	case j.cancelled:
		if res != nil {
			_ = os.Remove(res.path)
		}
		j.State = StateCancelled
		delete(m.jobs, j.ID)
		slog.Info("job cancelled", "job", j.ID, "channel", j.Channel)
	case err != nil:
		j.State = StateError
		j.Message = userMessage(err)
		slog.Warn("job failed", "job", j.ID, "channel", j.Channel, "start", j.Start, "err", err)
	default:
		j.State = StateReady
		j.video = res.path
		j.DownloadSeconds, j.TotalSeconds, j.RawMiB = res.downloadSeconds, res.totalSeconds, res.rawMiB
		j.Size, j.VideoDuration = res.size, res.duration
		slog.Info("job ready", "job", j.ID, "channel", j.Channel, "download_s", res.downloadSeconds,
			"raw_mib", res.rawMiB, "total_s", res.totalSeconds, "size", res.size)
	}
}

// expire frees results the caller never collected or deleted.
func (m *Manager) expire() {
	ttl := time.Duration(m.cfg.Limits.ResultTTL) * time.Second
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for id, j := range m.jobs {
		if j.State.active() || j.Finished == nil || now.Sub(*j.Finished) < ttl {
			continue
		}
		if j.video != "" {
			_ = os.Remove(j.video)
		}
		delete(m.jobs, id)
	}
}
