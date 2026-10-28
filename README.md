# dahua-transcoder

A small Go service that turns a time window of a **Dahua NVR** recording into
a compact, seekable **H.264 MP4**, on demand. Ask for "channel 3, from 21:14:30,
210 seconds" and it downloads that footage from the NVR, converts it (GPU
decode via VA-API, libx264 encode) and hands you the file over a
token-protected HTTP API.

It was written as the backend of a Home Assistant "explore this event" view:
the raw NVR stream is ~270 MiB for 3.5 minutes of a 4 MP camera, far too heavy
for a phone, and converting it inside Home Assistant took ~42 s. This service
does it in ~17 s. Nothing in it is Home Assistant specific, though: any HTTP
client can use it.

## Features

- **On-demand clips** from a Dahua NVR via `loadfile.cgi`, for any channel and
  window (up to 10 minutes by default).
- **Fast and light:** VA-API decode and scale on an Intel/AMD iGPU, libx264
  encode on the CPU, and conversion starts while the download is still
  running. Output fits inside 1920 px (configurable) keeping the aspect ratio,
  so portrait ("corridor mode") cameras stay portrait.
- **CPU-only mode** (`hwaccel: none`) for hosts without a GPU.
- **NVR friendly:** downloads at full speed and lets go of the NVR as early as
  possible; one job at a time; a running download can be cancelled instantly
  (for example when a live alert clip needs the recorder).
- **Digest auth with SHA-256** (what Dahua firmware offers), MD5 fallback.
- **Safe by default:** bearer token on every call, optional IP allow-list,
  size and duration checks (a window the NVR has not fully recorded yet is
  refused rather than returned truncated), error messages that never leak
  URLs, credentials or NVR responses.
- **Single static binary**, systemd unit with hardening included.

## How it works

```
client ──POST /v1/jobs──► dahua-transcoder ──loadfile.cgi (Digest)──► NVR
                              │  raw DHAV ──► disk (full speed)
                              │                 └─► pipe ─► ffmpeg: VA-API decode + scale ─► libx264
client ◄──GET .../video──────┘  MP4 (checked: duration, size)
```

1. **Download** the window to disk at full speed.
2. **Convert while downloading:** ffmpeg reads the same bytes through a pipe as
   they arrive.
3. **Check** the result: duration within ±3 s of the request (shorter means the
   NVR did not have the whole window yet), size under the cap.
4. **Serve** the MP4 until the client deletes it or `result_ttl_seconds` passes.

Jobs run **one at a time**: with two in parallel the NVR splits its bandwidth
and the CPU is shared, so both callers wait longer (measured ~45 s each)
than if the second queues behind the first (~17 s, then ~34 s).

## Requirements

- A Dahua NVR (or a rebrand running Dahua firmware) reachable over HTTP, and an
  account with **playback** rights on the channels you need.
- Linux with `ffmpeg` and `ffprobe` (tested with ffmpeg 7.1; the GPU path needs a `scale_vaapi`
  with `force_original_aspect_ratio`).
- For the GPU path: a VA-API render node (`/dev/dri/renderD128`) and its driver
  (`mesa-va-drivers` for AMD, `intel-media-va-driver` for recent Intel). Tested
  on an AMD Radeon 660M (Rembrandt) in a Proxmox LXC.
- About 1 GiB of RAM and 1 GiB of free disk (jobs run one at a time).

## Building

Requires Go 1.25+.

```sh
git clone https://github.com/paolostivanin/dahua-transcoder.git
cd dahua-transcoder
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dahua-transcoder .
./dahua-transcoder -version
```

## Installation

Tested on Debian 13. Run as root on the target machine.

### 1. GPU access (skip for `hwaccel: none`)

On bare metal or a VM with the GPU, the render node already exists. In a
**Proxmox LXC**, pass it in from the host: *CT > Resources > Add > Device
Passthrough*, path `/dev/dri/renderD128`, GID = the container's `render` group
(`getent group render` inside the CT; create it with `groupadd -r render` if
missing), mode `0660`. Restart the container.

```sh
apt install -y ffmpeg mesa-va-drivers vainfo    # Intel: intel-media-va-driver
vainfo --display drm --device /dev/dri/renderD128 2>&1 | grep H264
# expect: VAProfileH264High : VAEntrypointVLD
```

### 2. User, directories, binary

```sh
useradd --system --home-dir /var/lib/dahua-transcoder --shell /usr/sbin/nologin dahua-transcoder
install -d -m 0755 /opt/dahua-transcoder
install -d -m 0750 -g dahua-transcoder /etc/dahua-transcoder
install -m 0755 dahua-transcoder /opt/dahua-transcoder/dahua-transcoder
```

From another machine, copy the binary with `scp` first; into a Proxmox LXC,
`pct push <ctid> dahua-transcoder /opt/dahua-transcoder/dahua-transcoder --perms 0755`.

### 3. Configuration

```sh
install -m 0640 -g dahua-transcoder config.example.yaml /etc/dahua-transcoder/config.yaml
openssl rand -hex 32          # use this as the token
editor /etc/dahua-transcoder/config.yaml
```

Set at least `token`, `nvr.url`, `nvr.username`, `nvr.password`,
`nvr.timezone` and `allowed_ips`. The file holds the NVR password and the
token, so keep it `root:dahua-transcoder` `0640`.

### 4. systemd

```sh
install -m 0644 dahua-transcoder.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now dahua-transcoder
```

The unit runs the service as `dahua-transcoder` with the `render` group, at
`Nice=10`, with work files in `/var/lib/dahua-transcoder` (on disk, not `/tmp`:
Debian 13 keeps `/tmp` in RAM and one job writes ~270 MiB).

### 5. Verify

```sh
curl -s http://127.0.0.1:8080/healthz
# {"status":"ok","version":"0.1.0"}
curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/v1/status
# {"active":0,"hwaccel":"vaapi","jobs":0,"version":"0.1.0"}
journalctl -u dahua-transcoder -f
```

Then run one real job from the [Usage](#usage) section.

### Upgrading

Build the new binary, replace `/opt/dahua-transcoder/dahua-transcoder`, then
`systemctl restart dahua-transcoder`. A restart forgets all jobs: one that was
running disappears (`404`), so clients should resubmit.

## Configuration

`/etc/dahua-transcoder/config.yaml` (override with `CONFIG_FILE` or
`-config`). Unknown keys are rejected. See [`config.example.yaml`](config.example.yaml).

| Key | Default | |
|---|---|---|
| `listen` | `:8080` | HTTP listen address |
| `token` | *(required)* | Bearer token, at least 32 characters |
| `allowed_ips` | *(all)* | Client IPs or CIDRs allowed to connect |
| `nvr.url` | *(required)* | e.g. `http://192.168.1.20` |
| `nvr.username`, `nvr.password` | *(required)* | Playback-capable NVR account |
| `nvr.timezone` | `UTC` | The NVR's local time zone; `loadfile.cgi` takes wall-clock times |
| `nvr.max_channel` | `16` | Highest channel number accepted |
| `work_dir` | `/var/lib/dahua-transcoder` | Downloads and results |
| `hwaccel` | `vaapi` | `vaapi` or `none` |
| `vaapi_device` | `/dev/dri/renderD128` | |
| `max_long_edge` | `1920` | Output fits inside this square, aspect kept |
| `crf`, `preset`, `x264_threads` | `32`, `veryfast`, `4` | libx264 settings |
| `input_format` | `dhav` | What `loadfile.cgi` returns |
| `limits.max_queue` | `4` | Active + queued jobs |
| `limits.max_duration_seconds` | `600` | Longest window accepted |
| `limits.settle_seconds` | `10` | Windows ending less than this ago are refused (`409`) |
| `limits.raw_cap_mib`, `limits.output_cap_mib` | `512`, `128` | Size caps |
| `limits.download_timeout_seconds` | `90` | |
| `limits.transcode_timeout_seconds` | `240` | |
| `limits.job_timeout_seconds` | `360` | |
| `limits.result_ttl_seconds` | `600` | Uncollected results are deleted after this |
| `limits.duration_tolerance_seconds` | `3` | Shorter output = incomplete footage |

## NVR configuration

Create a dedicated account with the least rights that work. On recent Dahua
firmware: *Account > User > Add*, group `user`, and tick only the **Search**
(playback) right for the channels you need; no Live, Voice or System rights.
Downloading through `loadfile.cgi` needs nothing more. The NVR must be
reachable on TCP 80 from the service.

## Usage

The whole flow is: create a job, poll it, download the video, delete the job.

```sh
TOKEN=...; T=http://transcoder.lan:8080

# 1. Ask for channel 3, 210 s starting at a Unix timestamp
curl -s -H "Authorization: Bearer $TOKEN" -d '{"channel":3,"start":1767268800,"duration":210}' $T/v1/jobs
# {"id":"cbf8...","channel":3,"start":1767268800,"duration":210,"state":"queued",...}

# 2. Poll until "ready" (or "error")
curl -s -H "Authorization: Bearer $TOKEN" $T/v1/jobs/cbf8...
# {"id":"cbf8...","state":"downloading",...}
# {"id":"cbf8...","state":"ready","download_seconds":10.2,"total_seconds":17.6,
#  "raw_mib":266.8,"size":4096029,"video_duration":209.933,...}

# 3. Download the MP4 (byte ranges work, so it can be streamed to a player)
curl -s -H "Authorization: Bearer $TOKEN" -o clip.mp4 $T/v1/jobs/cbf8.../video

# 4. Free it (or let result_ttl_seconds expire it)
curl -s -X DELETE -H "Authorization: Bearer $TOKEN" $T/v1/jobs/cbf8...
```

Things worth knowing when writing a client:

- **Same request, same job.** Posting an identical channel/start/duration
  while a job exists returns that job instead of queueing another, so clients
  can retry freely.
- **Recent windows.** A window that ends less than `settle_seconds` ago is
  refused with `409 {"state":"waiting","available_at":<unix>}`. Wait until
  `available_at`, then post again.
- **Freeing the NVR.** While a job is `downloading` the NVR is busy with it.
  If something more urgent needs the recorder (a live alert clip, say),
  `DELETE` the job, which drops the NVR connection immediately, and post it
  again afterwards.
- **Errors** come back as `state: "error"` with a `message` meant for end users,
  e.g. *"The recorder returned incomplete footage for this interval."* or
  *"No recording is available for this channel and time."*

## API reference

All `/v1` calls need `Authorization: Bearer <token>`. `allowed_ips`, when set,
applies to every path.

| Method | Path | Result |
|---|---|---|
| GET | `/healthz` | `{"status":"ok","version":"..."}`, no token |
| GET | `/v1/status` | `{"version","hwaccel","active","jobs"}` |
| POST | `/v1/jobs` | `{"channel":1,"start":<unix>,"duration":210}` → job |
| GET | `/v1/jobs/{id}` | job |
| GET | `/v1/jobs/{id}/video` | `video/mp4` once ready (`409` before) |
| DELETE | `/v1/jobs/{id}` | `204`: cancels a running job or frees a finished one |

Job states: `queued` → `downloading` (NVR in use, conversion already running)
→ `transcoding` (download done) → `ready` or `error`.

Status codes: `400` invalid request, `401` bad token, `403` IP not allowed,
`404` unknown job, `409` window not finished / video not ready, `429` queue
full, `503` shutting down.

## Performance

Measured on an AMD Radeon 660M iGPU (4-core Proxmox LXC), 210 s of a 4 MP
channel (~267 MiB of DHAV):

| Pipeline | Time incl. download | Quality (SSIM) | Output |
|---|---|---|---|
| CPU only, download then convert | ~42 s | 0.955 | 3.9 MiB |
| **This: GPU decode + libx264, overlapped** | **~17 s** | **0.950** | 3.9 MiB |
| GPU encode (`h264_vaapi`) | ~23 s | 0.905 at the same size | 3.9 MiB |

The AMD H.264 hardware encoder was visibly worse at this bitrate and no faster
(and two hardware encodes serialize), hence libx264. Memory peaks at ~400 MiB
for one job; idle it uses ~20 MiB.

## Troubleshooting

- **`The recorder rejected the login`**: wrong credentials, or the account lacks
  playback rights on that channel.
- **`No recording is available for this channel and time`**: the NVR has no
  footage there (outside retention, or the channel was not recording).
- **`incomplete footage`**: the window was not fully recorded yet, or has a gap.
  Retry after `available_at`; increase `settle_seconds` if your NVR flushes
  slowly.
- **`The recording could not be converted`**: see `journalctl -u dahua-transcoder`
  for ffmpeg's error. Check `vainfo`, and that the render node belongs to group
  `render` with mode `0660` (the service runs with that group), or try `hwaccel: none`.
- **Clients with their own Digest code fail against the NVR but this works**:
  Dahua offers SHA-256 Digest, which some libraries (e.g. Python's urllib
  before 3.14) cannot answer.

## Security notes

- Plain HTTP: run it on a trusted LAN, or put it behind a TLS reverse proxy.
- Always set `allowed_ips` to the clients that need it.
- The token is compared in constant time; the NVR password is only sent to
  the NVR as a Digest response.
- Give the NVR account playback rights only.

## Development

```sh
gofmt -l . && go vet ./... && go test -race ./...
```

The tests need no NVR and no GPU: a fake NVR implements Digest auth (checked
against the RFC 7616 example), fake `ffmpeg`/`ffprobe` scripts verify that
every recorded byte reaches ffmpeg in order, and one test runs the real
`ffmpeg` on a generated clip when it is installed.

## License

MIT - see [LICENSE](LICENSE).
