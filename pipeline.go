package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// jobError carries a message that is safe to show to the caller: no URLs,
// credentials or recorder responses. The wrapped error is only logged.
type jobError struct {
	msg string
	err error
}

func (e *jobError) Error() string {
	if e.err != nil {
		return e.msg + ": " + e.err.Error()
	}
	return e.msg
}

func (e *jobError) Unwrap() error { return e.err }

func fail(msg string, err error) error { return &jobError{msg: msg, err: err} }

func userMessage(err error) string {
	var je *jobError
	if errors.As(err, &je) {
		return je.msg + "."
	}
	return "Video preparation failed."
}

type result struct {
	path            string
	downloadSeconds float64
	totalSeconds    float64
	rawMiB          float64
	size            int64
	duration        float64
}

// loadfileURL builds the NVR request. The NVR takes local wall-clock times
// without an offset, with the space encoded and the colons literal.
func (m *Manager) loadfileURL(channel int, start int64, duration int) string {
	f := func(t int64) string {
		return strings.Replace(time.Unix(t, 0).In(m.cfg.location).Format("2006-01-02 15:04:05"), " ", "%20", 1)
	}
	return strings.TrimRight(m.cfg.NVR.URL, "/") + "/cgi-bin/loadfile.cgi?action=startLoad&channel=" +
		strconv.Itoa(channel) + "&startTime=" + f(start) + "&endTime=" + f(start+int64(duration))
}

func (m *Manager) ffmpegArgs(duration int, out string) []string {
	c := m.cfg
	edge := strconv.Itoa(c.MaxLongEdge)
	fit := "w=" + edge + ":h=" + edge + ":force_original_aspect_ratio=decrease:force_divisible_by=2"
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y"}
	var vf string
	if c.HWAccel == "vaapi" {
		args = append(args, "-hwaccel", "vaapi", "-hwaccel_device", c.VAAPIDevice, "-hwaccel_output_format", "vaapi")
		vf = "scale_vaapi=" + fit + ":format=nv12,hwdownload,format=nv12,setpts=PTS-STARTPTS"
	} else {
		vf = "scale=" + fit + ",setpts=PTS-STARTPTS"
	}
	if c.InputFormat != "" {
		args = append(args, "-f", c.InputFormat)
	}
	args = append(args, "-i", "pipe:0", "-map", "0:v:0", "-an", "-vf", vf,
		"-c:v", "libx264", "-preset", c.Preset, "-crf", strconv.Itoa(c.CRF), "-pix_fmt", "yuv420p")
	if c.X264Threads > 0 {
		args = append(args, "-threads", strconv.Itoa(c.X264Threads))
	}
	return append(args, "-t", strconv.Itoa(duration), "-fs", strconv.FormatInt(c.Limits.OutputCapMiB<<20, 10),
		"-movflags", "+faststart", "-f", "mp4", out)
}

// process downloads one window and converts it. The download writes to disk
// at full speed so the NVR is released as early as possible, while ffmpeg
// reads the same bytes through a pipe as they arrive. A pipe rather than
// ffmpeg's own file "follow" mode: on a growing DHAV file that mode measured
// no overlap at all plus a read-timeout stall at the end.
func (m *Manager) process(ctx context.Context, j *Job) (*result, error) {
	l := m.cfg.Limits
	began := time.Now()
	raw := filepath.Join(m.cfg.WorkDir, j.ID+".dav")
	part := filepath.Join(m.cfg.WorkDir, j.ID+".part.mp4")
	final := filepath.Join(m.cfg.WorkDir, j.ID+".mp4")
	defer os.Remove(raw)
	defer os.Remove(part)

	out, err := os.OpenFile(raw, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fail("Could not create a work file", err)
	}
	dlCtx, dlCancel := context.WithTimeout(ctx, time.Duration(l.DownloadTimeout)*time.Second)
	defer dlCancel()
	progress := make(chan struct{}, 1)
	dlDone := make(chan struct{})
	var (
		dlErr     error
		rawBytes  int64
		dlSeconds float64
	)
	go func() {
		defer close(dlDone)
		t0 := time.Now()
		rawBytes, dlErr = m.download(dlCtx, j, out, progress)
		if cerr := out.Close(); dlErr == nil && cerr != nil {
			dlErr = fail("Could not write the recording", cerr)
		}
		dlSeconds = time.Since(t0).Seconds()
	}()

	tcCtx, tcCancel := context.WithTimeout(ctx, time.Duration(l.TranscodeTimeout)*time.Second)
	defer tcCancel()
	cmd := exec.CommandContext(tcCtx, m.cfg.FFmpeg, m.ffmpegArgs(j.Duration, part)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	stderr := &tailBuffer{max: 2048}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		dlCancel()
		<-dlDone
		return nil, fail("Could not start ffmpeg", err)
	}
	feedDone := make(chan error, 1)
	go func() { feedDone <- feed(tcCtx, raw, stdin, dlDone, progress) }()
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	// stop tears everything down and reports the most specific cause.
	stop := func(cause error) (*result, error) {
		dlCancel()
		tcCancel()
		<-dlDone
		<-waitDone
		<-feedDone
		return nil, m.contextCause(ctx, cause)
	}

	var werr error
	ffmpegExited := false
	select {
	case <-dlDone:
	case werr = <-waitDone:
		ffmpegExited = true
	}
	if ffmpegExited && werr != nil {
		slog.Warn("ffmpeg failed during download", "job", j.ID, "stderr", stderr.String())
		dlCancel()
		<-dlDone
		<-feedDone
		return nil, m.contextCause(ctx, fail("The recording could not be converted", werr))
	}
	<-dlDone
	if dlErr != nil {
		if errors.Is(dlErr, context.DeadlineExceeded) && ctx.Err() == nil {
			dlErr = fail("The recorder download timed out", dlErr)
		}
		if ffmpegExited {
			<-feedDone
			return nil, m.contextCause(ctx, dlErr)
		}
		return stop(dlErr)
	}
	m.setState(j, StateTranscoding)
	if !ffmpegExited {
		werr = <-waitDone
	}
	ferr := <-feedDone
	if ctx.Err() != nil {
		return nil, m.contextCause(ctx, nil)
	}
	if werr != nil {
		if errors.Is(tcCtx.Err(), context.DeadlineExceeded) {
			return nil, fail("Video conversion timed out", werr)
		}
		slog.Warn("ffmpeg failed", "job", j.ID, "stderr", stderr.String())
		return nil, fail("The recording could not be converted", werr)
	}
	if ferr != nil {
		return nil, fail("Could not feed the recording to ffmpeg", ferr)
	}

	duration, err := m.probe(ctx, part)
	if err != nil {
		return nil, fail("The converted video could not be checked", err)
	}
	if math.Abs(duration-float64(j.Duration)) > float64(l.DurationTolerance) {
		return nil, fail("The recorder returned incomplete footage for this interval",
			fmt.Errorf("got %.1fs of %ds", duration, j.Duration))
	}
	info, err := os.Stat(part)
	if err != nil {
		return nil, fail("The converted video could not be checked", err)
	}
	if info.Size() <= 0 || info.Size() > l.OutputCapMiB<<20 {
		return nil, fail("The converted video exceeded the size limit", nil)
	}
	if err := os.Rename(part, final); err != nil {
		return nil, fail("Could not store the converted video", err)
	}
	return &result{
		path:            final,
		downloadSeconds: math.Round(dlSeconds*10) / 10,
		totalSeconds:    math.Round(time.Since(began).Seconds()*10) / 10,
		rawMiB:          math.Round(float64(rawBytes)/(1<<20)*10) / 10,
		size:            info.Size(),
		duration:        duration,
	}, nil
}

// contextCause maps a cancelled or expired job context to a clear message;
// otherwise it returns cause unchanged.
func (m *Manager) contextCause(ctx context.Context, cause error) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fail("Video preparation timed out", ctx.Err())
	case errors.Is(ctx.Err(), context.Canceled):
		return fail("Video preparation was cancelled", ctx.Err())
	case cause == nil:
		return fail("Video preparation failed", nil)
	}
	return cause
}

func (m *Manager) download(ctx context.Context, j *Job, w io.Writer, progress chan<- struct{}) (int64, error) {
	resp, err := m.nvr.Get(ctx, m.loadfileURL(j.Channel, j.Start, j.Duration))
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, fail("Could not reach the recorder", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 401:
		return 0, fail("The recorder rejected the login", nil)
	case resp.StatusCode != 200:
		return 0, fail(fmt.Sprintf("The recorder returned HTTP %d", resp.StatusCode), nil)
	}
	limit := m.cfg.Limits.RawCapMiB << 20
	buf := make([]byte, 256<<10)
	var total int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > limit {
				return total, fail("The recording exceeded the download size limit", nil)
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return total, fail("Could not write the recording", werr)
			}
			select {
			case progress <- struct{}{}:
			default:
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if ctx.Err() != nil {
				return total, ctx.Err()
			}
			return total, fail("The recorder download was interrupted", rerr)
		}
	}
	if total == 0 {
		return 0, fail("No recording is available for this channel and time", nil)
	}
	return total, nil
}

// feed copies the growing file at path into w until the download is done and
// every byte has been sent, then closes w so ffmpeg sees end of input. A
// broken pipe is not an error here: ffmpeg may stop reading once it has
// produced the requested duration, and its own exit status decides.
func feed(ctx context.Context, path string, w io.WriteCloser, done <-chan struct{}, progress <-chan struct{}) error {
	defer w.Close()
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	finishing := false
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				if errors.Is(werr, syscall.EPIPE) || errors.Is(werr, os.ErrClosed) {
					return nil
				}
				return werr
			}
			continue
		}
		if err != nil && err != io.EOF {
			return err
		}
		if finishing {
			return nil
		}
		select {
		case <-done:
			finishing = true // one more pass picks up bytes written since the last read
		case <-progress:
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (m *Manager) probe(ctx context.Context, path string) (float64, error) {
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(pctx, m.cfg.FFprobe, "-v", "error", "-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", path).Output()
	if err != nil {
		return 0, err
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || math.IsNaN(d) || math.IsInf(d, 0) {
		return 0, fmt.Errorf("unusable duration %q", strings.TrimSpace(string(out)))
	}
	return d, nil
}

// tailBuffer keeps the last max bytes written, for logging ffmpeg errors.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.b))
}
