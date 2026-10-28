# CLAUDE.md

Guidance for working on this repository.

## Project overview

dahua-transcoder is a single-binary Go service that downloads a time window
from a Dahua NVR and converts it into a phone-friendly MP4 for Home
Assistant's event review (a Home Assistant custom integration calls it),
though the HTTP API is generic. See README.md for the API and the measured
reasons behind the pipeline.

Files: `main.go` (config, startup), `digest.go` (RFC 7616 Digest client),
`jobs.go` (job table, queue, single worker, expiry), `pipeline.go` (download,
pipe feed, ffmpeg, checks), `api.go` (HTTP API).

## Build, test, deploy

- Test: `go test -race ./...` (no NVR or GPU needed; the real-ffmpeg test
  skips when ffmpeg is absent). Also `gofmt -l .` and `go vet ./...`.
- Build for the container: `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dahua-transcoder .`
- Production: `/opt/dahua-transcoder/dahua-transcoder`, config
  `/etc/dahua-transcoder/config.yaml` (`CONFIG_FILE` overrides), systemd
  `dahua-transcoder.service`, work dir `/var/lib/dahua-transcoder`.

## Invariants - keep these unless new measurements say otherwise

- **One job at a time.** Parallel jobs were measured slower for every caller.
- **Hybrid pipeline:** VA-API decode + `scale_vaapi`, then `hwdownload` and
  libx264. The radeonsi `h264_vaapi` encoder was measured worse quality at
  the same size and no faster.
- **Overlap via a pipe fed from the growing file**, not ffmpeg `-follow 1`
  (measured: no overlap plus a timeout stall on DHAV). The download keeps
  writing at full speed; it never waits for ffmpeg, so the NVR is released
  early. `feed()` must make one more read pass after the download finishes;
  `TestFeedGrowingFile` forces that interleaving.
- **Digest auth prefers SHA-256.** The NVR offers it; Python < 3.14 urllib
  cannot answer it, which is why this does not shell out to such clients.
- **Never log or return secrets**: no NVR URL with credentials, no token, no
  NVR response bodies in user-facing messages (`jobError.msg`).
- Work files on disk under the state dir, never `/tmp` (tmpfs on Debian 13).
- Keep house-specific values (IPs, users, time zone) in the config file, not
  in code or defaults.
