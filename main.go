// Command dahua-transcoder prepares Dahua NVR recordings for Home Assistant's
// event review. On request it downloads a time window from the NVR and
// converts it into a small, seekable MP4: decode and scale on a VA-API GPU,
// encode with libx264 on the CPU (the GPU's own H.264 encoder was measured
// visibly worse at the same size). Conversion starts while the download is
// still running, fed through a pipe from the file being written, so the NVR
// is released at full download speed and the two phases overlap.
//
// Jobs run one at a time on purpose: two parallel jobs split the NVR's
// bandwidth and the CPU, so both callers wait longer than if the second
// simply queued behind the first.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // NVR local time must resolve even without system zoneinfo

	"gopkg.in/yaml.v3"
)

const version = "0.1.0"

// Config is read from /etc/dahua-transcoder/config.yaml (CONFIG_FILE
// overrides the path). It holds the NVR password and the API token, so the
// file must not be world-readable.
type Config struct {
	Listen     string   `yaml:"listen"`
	Token      string   `yaml:"token"`
	AllowedIPs []string `yaml:"allowed_ips"`
	NVR        struct {
		URL        string `yaml:"url"`
		Username   string `yaml:"username"`
		Password   string `yaml:"password"`
		Timezone   string `yaml:"timezone"`
		MaxChannel int    `yaml:"max_channel"`
	} `yaml:"nvr"`
	WorkDir     string `yaml:"work_dir"`
	FFmpeg      string `yaml:"ffmpeg"`
	FFprobe     string `yaml:"ffprobe"`
	InputFormat string `yaml:"input_format"`
	HWAccel     string `yaml:"hwaccel"`
	VAAPIDevice string `yaml:"vaapi_device"`
	MaxLongEdge int    `yaml:"max_long_edge"`
	CRF         int    `yaml:"crf"`
	Preset      string `yaml:"preset"`
	X264Threads int    `yaml:"x264_threads"`
	Limits      struct {
		MaxQueue          int   `yaml:"max_queue"`
		MaxDuration       int   `yaml:"max_duration_seconds"`
		SettleSeconds     int   `yaml:"settle_seconds"`
		RawCapMiB         int64 `yaml:"raw_cap_mib"`
		OutputCapMiB      int64 `yaml:"output_cap_mib"`
		DownloadTimeout   int   `yaml:"download_timeout_seconds"`
		TranscodeTimeout  int   `yaml:"transcode_timeout_seconds"`
		JobTimeout        int   `yaml:"job_timeout_seconds"`
		ResultTTL         int   `yaml:"result_ttl_seconds"`
		DurationTolerance int   `yaml:"duration_tolerance_seconds"`
	} `yaml:"limits"`

	location *time.Location
	allowed  []netip.Prefix
}

func defaultConfig() Config {
	var c Config
	c.Listen = ":8080"
	c.NVR.Timezone = "UTC"
	c.NVR.MaxChannel = 16
	c.WorkDir = "/var/lib/dahua-transcoder"
	c.FFmpeg = "ffmpeg"
	c.FFprobe = "ffprobe"
	c.InputFormat = "dhav"
	c.HWAccel = "vaapi"
	c.VAAPIDevice = "/dev/dri/renderD128"
	c.MaxLongEdge = 1920
	c.CRF = 32
	c.Preset = "veryfast"
	c.X264Threads = 4
	c.Limits.MaxQueue = 4
	c.Limits.MaxDuration = 600
	c.Limits.SettleSeconds = 10
	// 210 s of a 4 MP channel measured 267 MiB (10.6 Mbps); the cap leaves
	// room for busier scenes without letting one job fill the disk.
	c.Limits.RawCapMiB = 512
	c.Limits.OutputCapMiB = 128
	c.Limits.DownloadTimeout = 90
	c.Limits.TranscodeTimeout = 240
	c.Limits.JobTimeout = 360
	c.Limits.ResultTTL = 600
	c.Limits.DurationTolerance = 3
	return c
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := defaultConfig()
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o004 != 0 {
		slog.Warn("config file is world-readable; it holds the NVR password and API token", "path", path)
	}
	return &c, nil
}

func (c *Config) validate() error {
	var problems []string
	if len(c.Token) < 32 {
		problems = append(problems, "token must be at least 32 characters")
	}
	if c.NVR.URL == "" || c.NVR.Username == "" || c.NVR.Password == "" {
		problems = append(problems, "nvr.url, nvr.username and nvr.password are required")
	}
	loc, err := time.LoadLocation(c.NVR.Timezone)
	if err != nil {
		problems = append(problems, "nvr.timezone: "+err.Error())
	}
	c.location = loc
	if c.NVR.MaxChannel < 1 {
		problems = append(problems, "nvr.max_channel must be >= 1")
	}
	if !filepath.IsAbs(c.WorkDir) {
		problems = append(problems, "work_dir must be an absolute path")
	}
	if c.HWAccel != "vaapi" && c.HWAccel != "none" {
		problems = append(problems, `hwaccel must be "vaapi" or "none"`)
	}
	if c.MaxLongEdge < 2 || c.MaxLongEdge%2 != 0 {
		problems = append(problems, "max_long_edge must be a positive even number")
	}
	l := c.Limits
	for name, v := range map[string]int{
		"limits.max_queue": l.MaxQueue, "limits.max_duration_seconds": l.MaxDuration,
		"limits.download_timeout_seconds": l.DownloadTimeout, "limits.transcode_timeout_seconds": l.TranscodeTimeout,
		"limits.job_timeout_seconds": l.JobTimeout, "limits.result_ttl_seconds": l.ResultTTL,
	} {
		if v < 1 {
			problems = append(problems, name+" must be >= 1")
		}
	}
	if l.RawCapMiB < 1 || l.OutputCapMiB < 1 {
		problems = append(problems, "limits.raw_cap_mib and limits.output_cap_mib must be >= 1")
	}
	c.allowed = nil
	for _, s := range c.AllowedIPs {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			a, aerr := netip.ParseAddr(s)
			if aerr != nil {
				problems = append(problems, "allowed_ips: invalid entry "+s)
				continue
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		c.allowed = append(c.allowed, p.Masked())
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func main() {
	defaultPath := os.Getenv("CONFIG_FILE")
	if defaultPath == "" {
		defaultPath = "/etc/dahua-transcoder/config.yaml"
	}
	configPath := flag.String("config", defaultPath, "path to config.yaml")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("dahua-transcoder", version)
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))

	cfg, err := loadConfig(*configPath)
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	m, err := NewManager(cfg)
	if err != nil {
		slog.Error("startup", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go m.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           newAPI(m),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	slog.Info("dahua-transcoder starting", "version", version, "listen", cfg.Listen, "hwaccel", cfg.HWAccel)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("http server", "err", err)
		os.Exit(1)
	}
	m.Wait()
}
