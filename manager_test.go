package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Fake ffmpeg: copies stdin to the last argument, so a successful job's
// output must equal the recorder's bytes - proof that the growing-file feed
// delivered everything, in order.
const fakeFFmpeg = `#!/bin/sh
for a; do out="$a"; done
[ -n "$FAKE_FFMPEG_ARGS" ] && printf '%s\n' "$@" > "$FAKE_FFMPEG_ARGS"
cat > "$out" || exit 1
exit "${FAKE_FFMPEG_EXIT:-0}"
`

const fakeFFprobe = `#!/bin/sh
echo "${FAKE_DURATION:-210.0}"
`

const testStart = int64(1767268800) // 2026-01-01 12:00:00 UTC

type harness struct {
	t     *testing.T
	m     *Manager
	nvr   *digestServer
	srv   *httptest.Server
	cfg   *Config
	stop  context.CancelFunc
	clock *fakeClock
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Add(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func testConfig(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	c := defaultConfig()
	c.Token = strings.Repeat("t", 40)
	c.NVR.URL = "http://placeholder"
	c.NVR.Username = "viewer"
	c.NVR.Password = "secret"
	c.NVR.Timezone = "UTC"
	c.NVR.MaxChannel = 6
	c.WorkDir = filepath.Join(dir, "work")
	c.FFmpeg = writeScript(t, dir, "ffmpeg", fakeFFmpeg)
	c.FFprobe = writeScript(t, dir, "ffprobe", fakeFFprobe)
	c.HWAccel = "none"
	c.Limits.SettleSeconds = 10
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	return &c
}

func newHarness(t *testing.T, cfg *Config, body func(w http.ResponseWriter, r *http.Request)) *harness {
	t.Helper()
	if cfg == nil {
		cfg = testConfig(t)
	}
	nvr := &digestServer{user: cfg.NVR.Username, pass: cfg.NVR.Password, algs: []string{"MD5", "SHA-256"}, body: body}
	srv := httptest.NewServer(nvr)
	cfg.NVR.URL = srv.URL
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: time.Unix(testStart+3600, 0)}
	m.now = clock.Now
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	h := &harness{t: t, m: m, nvr: nvr, srv: srv, cfg: cfg, stop: cancel, clock: clock}
	t.Cleanup(func() {
		cancel()
		m.Wait()
		srv.Close()
	})
	return h
}

func serveBytes(b []byte) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) { w.Write(b) }
}

// serveSlowly streams b in n chunks with a pause between them, so the
// conversion demonstrably starts before the download ends.
func serveSlowly(b []byte, n int, pause time.Duration) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		step := (len(b) + n - 1) / n
		for i := 0; i < len(b); i += step {
			end := min(i+step, len(b))
			if _, err := w.Write(b[i:end]); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-time.After(pause):
			case <-r.Context().Done():
				return
			}
		}
	}
}

func randomBytes(t *testing.T, n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func (h *harness) wait(id string, states ...State) Job {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		j, err := h.m.Get(id)
		if err != nil {
			h.t.Fatalf("job %s: %v", id, err)
		}
		for _, s := range states {
			if j.State == s {
				return j
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	j, _ := h.m.Get(id)
	h.t.Fatalf("job %s stuck in %s (%s), want %v", id, j.State, j.Message, states)
	return Job{}
}

func workFiles(t *testing.T, dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestJobSuccessDeliversEveryByte(t *testing.T) {
	data := randomBytes(t, 3<<20+12345)
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("FAKE_FFMPEG_ARGS", argsFile)
	h := newHarness(t, nil, serveSlowly(data, 12, 20*time.Millisecond))

	j, err := h.m.Submit(1, testStart, 210)
	if err != nil {
		t.Fatal(err)
	}
	done := h.wait(j.ID, StateReady, StateError)
	if done.State != StateReady {
		t.Fatalf("job failed: %s", done.Message)
	}
	path, _, _ := h.m.Video(j.ID)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("output differs from the recording: %d vs %d bytes", len(got), len(data))
	}
	if want := "action=startLoad&channel=1&startTime=2026-01-01%2012:00:00&endTime=2026-01-01%2012:03:30"; h.nvr.lastQuery != want {
		t.Errorf("NVR query = %q, want %q", h.nvr.lastQuery, want)
	}
	if done.RawMiB < 3 || done.Size != int64(len(data)) || done.VideoDuration != 210 {
		t.Errorf("unexpected stats: %+v", done)
	}
	args, _ := os.ReadFile(argsFile)
	for _, want := range []string{"pipe:0", "libx264", "-t\n210\n", "+faststart"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("ffmpeg args lack %q:\n%s", want, args)
		}
	}
	if err := h.m.Delete(j.ID); err != nil {
		t.Fatal(err)
	}
	if left := workFiles(t, h.cfg.WorkDir); len(left) != 0 {
		t.Errorf("work dir not empty after delete: %v", left)
	}
}

func TestVAAPIArguments(t *testing.T) {
	c := testConfig(t)
	c.HWAccel = "vaapi"
	m := &Manager{cfg: c}
	args := strings.Join(m.ffmpegArgs(210, "/out.mp4"), " ")
	for _, want := range []string{
		"-hwaccel vaapi -hwaccel_device /dev/dri/renderD128 -hwaccel_output_format vaapi -f dhav -i pipe:0",
		"scale_vaapi=w=1920:h=1920:force_original_aspect_ratio=decrease:force_divisible_by=2:format=nv12,hwdownload,format=nv12,setpts=PTS-STARTPTS",
		"-c:v libx264 -preset veryfast -crf 32 -pix_fmt yuv420p -threads 4 -t 210 -fs 134217728",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %q:\n%s", want, args)
		}
	}
}

func TestJobFailures(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		rawCap  int64
		body    func(w http.ResponseWriter, r *http.Request)
		message string
	}{
		{"incomplete footage", map[string]string{"FAKE_DURATION": "120.0"}, 0, serveBytes([]byte("x")), "incomplete footage"},
		{"recorder http error", nil, 0, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }, "HTTP 404"},
		{"empty recording", nil, 0, serveBytes(nil), "No recording is available"},
		{"raw cap", nil, 1, serveBytes(make([]byte, 2<<20)), "size limit"},
		{"ffmpeg failure", map[string]string{"FAKE_FFMPEG_EXIT": "1"}, 0, serveBytes([]byte("x")), "could not be converted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg := testConfig(t)
			if tc.rawCap > 0 {
				cfg.Limits.RawCapMiB = tc.rawCap
			}
			h := newHarness(t, cfg, tc.body)
			j, err := h.m.Submit(2, testStart, 210)
			if err != nil {
				t.Fatal(err)
			}
			done := h.wait(j.ID, StateReady, StateError)
			if done.State != StateError || !strings.Contains(done.Message, tc.message) {
				t.Fatalf("got %s %q, want error containing %q", done.State, done.Message, tc.message)
			}
			if left := workFiles(t, cfg.WorkDir); len(left) != 0 {
				t.Errorf("leftover work files: %v", left)
			}
		})
	}
}

func TestCancelDuringDownload(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	h := newHarness(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	j, _ := h.m.Submit(1, testStart, 210)
	h.wait(j.ID, StateDownloading)
	time.Sleep(50 * time.Millisecond)
	if err := h.m.Delete(j.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := h.m.Get(j.ID); errors.Is(err, errNotFound) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := h.m.Get(j.ID); !errors.Is(err, errNotFound) {
		t.Fatal("cancelled job still present")
	}
	if left := workFiles(t, h.cfg.WorkDir); len(left) != 0 {
		t.Errorf("leftover work files after cancel: %v", left)
	}
}

func TestDedupeAndQueueLimit(t *testing.T) {
	release := make(chan struct{})
	cfg := testConfig(t)
	cfg.Limits.MaxQueue = 2
	h := newHarness(t, cfg, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
			w.Write([]byte("x"))
		case <-r.Context().Done():
		}
	})
	a, err := h.m.Submit(1, testStart, 210)
	if err != nil {
		t.Fatal(err)
	}
	h.wait(a.ID, StateDownloading)
	again, _ := h.m.Submit(1, testStart, 210)
	if again.ID != a.ID {
		t.Errorf("same interval got a second job %s", again.ID)
	}
	b, err := h.m.Submit(2, testStart, 210)
	if err != nil || b.State != StateQueued {
		t.Fatalf("second job: %+v %v", b, err)
	}
	if _, err := h.m.Submit(3, testStart, 210); !errors.Is(err, errQueueFull) {
		t.Fatalf("third job: got %v, want errQueueFull", err)
	}
	if err := h.m.Delete(b.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	h.wait(a.ID, StateReady, StateError)
}

func TestSubmitValidation(t *testing.T) {
	h := newHarness(t, nil, serveBytes([]byte("x")))
	var inv *invalidError
	for _, tc := range []struct {
		ch  int
		st  int64
		dur int
	}{{0, testStart, 210}, {7, testStart, 210}, {1, testStart, 0}, {1, testStart, 601}, {1, 5, 210}} {
		if _, err := h.m.Submit(tc.ch, tc.st, tc.dur); !errors.As(err, &inv) {
			t.Errorf("Submit(%d,%d,%d) = %v, want invalidError", tc.ch, tc.st, tc.dur, err)
		}
	}
	var notYet *notYetError
	now := h.clock.Now().Unix()
	if _, err := h.m.Submit(1, now-100, 210); !errors.As(err, &notYet) || notYet.availableAt != now+120 {
		t.Errorf("recent window: got %v (%+v), want notYetError at %d", err, notYet, now+120)
	}
}

func TestExpireAndStartupCleanup(t *testing.T) {
	h := newHarness(t, nil, serveBytes([]byte("x")))
	j, _ := h.m.Submit(1, testStart, 210)
	h.wait(j.ID, StateReady)
	h.m.expire()
	if _, err := h.m.Get(j.ID); err != nil {
		t.Fatal("expired too early")
	}
	h.clock.Add(11 * time.Minute)
	h.m.expire()
	if _, err := h.m.Get(j.ID); !errors.Is(err, errNotFound) {
		t.Fatal("not expired after the TTL")
	}
	if left := workFiles(t, h.cfg.WorkDir); len(left) != 0 {
		t.Errorf("expired video left behind: %v", left)
	}

	dir := t.TempDir()
	for _, n := range []string{"a.dav", "b.part.mp4", "c.mp4", "keep.txt"} {
		os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600)
	}
	if err := cleanWorkDir(dir); err != nil {
		t.Fatal(err)
	}
	if left := workFiles(t, dir); len(left) != 1 || left[0] != "keep.txt" {
		t.Errorf("cleanWorkDir left %v", left)
	}
}

func TestFeedGrowingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raw")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	data := randomBytes(t, 5<<20)
	tail := len(data) - 4096
	done := make(chan struct{})
	progress := make(chan struct{}, 1)
	go func() {
		defer close(done)
		for i := 0; i < tail; i += 300 << 10 {
			f.Write(data[i:min(i+300<<10, tail)])
			select {
			case progress <- struct{}{}:
			default:
			}
			time.Sleep(5 * time.Millisecond)
		}
		// Let the feeder go idle at end of file, then write the last chunk
		// with no progress signal and finish at once: only the final read
		// pass after "done" can deliver these bytes.
		time.Sleep(120 * time.Millisecond)
		f.Write(data[tail:])
		f.Close()
	}()
	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() { errc <- feed(context.Background(), path, pw, done, progress) }()
	got, err := io.ReadAll(pr)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("fed %d bytes, want %d identical bytes", len(got), len(data))
	}
}

// TestRealFFmpegSoftware runs the real ffmpeg/ffprobe (software path) on a
// generated MPEG-TS clip, to check the actual argument list end to end.
func TestRealFFmpegSoftware(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	if testing.Short() {
		t.Skip("short mode")
	}
	clip, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i",
		"testsrc2=s=320x568:r=15", "-t", "8", "-c:v", "libx264", "-preset", "ultrafast", "-f", "mpegts", "pipe:1").Output()
	if err != nil {
		t.Fatalf("generate clip: %v", err)
	}
	cfg := testConfig(t)
	cfg.FFmpeg, cfg.FFprobe = ffmpeg, ffprobe
	cfg.InputFormat = "mpegts"
	cfg.MaxLongEdge = 256
	cfg.X264Threads = 1
	h := newHarness(t, cfg, serveSlowly(clip, 4, 30*time.Millisecond))
	j, err := h.m.Submit(3, testStart, 8)
	if err != nil {
		t.Fatal(err)
	}
	done := h.wait(j.ID, StateReady, StateError)
	if done.State != StateReady {
		t.Fatalf("real ffmpeg job failed: %s", done.Message)
	}
	path, _, _ := h.m.Video(j.ID)
	out, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "v:0", "-show_entries",
		"stream=width,height,codec_name", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "h264,144,256" {
		t.Errorf("output stream = %q, want h264,144,256 (portrait fitted into 256)", got)
	}
}
