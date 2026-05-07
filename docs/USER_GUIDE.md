# Open Streamer — User Guide

How to install, configure, and operate Open Streamer end-to-end.

> Companion docs: [CONFIG.md](./CONFIG.md) (every config field + examples) ·
> [ARCHITECTURE.md](./ARCHITECTURE.md) (how it works) ·
> [APP_FLOW.md](./APP_FLOW.md) (pipeline + events) ·
> [FEATURES_CHECKLIST.md](./FEATURES_CHECKLIST.md) (what's implemented).

---

## 1. Install

### Binary release (recommended)

Latest release ships pre-built archives + Linux installer:

```bash
# Linux/systemd installer — downloads + verifies + installs as a service.
# Idempotent: re-running with a new tag uninstalls cleanly first.
sudo bash <(curl -sL https://raw.githubusercontent.com/ntt0601zcoder/open-streamer/main/build/reinstall.sh) v1.0.0
```

This installs the binary to `/usr/local/bin/open-streamer`, a systemd unit
to `/etc/systemd/system/open-streamer.service`, and creates the data dir
at `/var/lib/open-streamer` (preserved across version upgrades).

### From source

```bash
git clone https://github.com/ntt0601zcoder/open-streamer.git
cd open-streamer
make build          # → bin/open-streamer
make run            # run without persisting binary
```

Requires Go 1.25.9+. FFmpeg is required for transcoding (see § 2).

### Docker

```bash
make docker-build   # → open-streamer:local
make compose-up     # docker compose up
```

---

## 2. FFmpeg

Open Streamer spawns FFmpeg only for transcoding (ingest is pure Go). At
boot the server probes `transcoder.ffmpeg_path` (or `ffmpeg` from
`$PATH`) and fails fast on missing required encoders:

- **Required**: `libx264`, `aac`, muxer `mpegts`
- **Optional warnings**: `h264_nvenc`, `hevc_nvenc` (NVENC), `libx265`,
  `libvpx-vp9`, `libsvtav1`, `libopus`, `libmp3lame`, `ac3`, muxers
  `hls` / `dash`

Probe an arbitrary binary live via `POST /api/v1/config/transcoder/probe`
or the UI's "Test FFmpeg" button before saving the path.

Recommended: ffmpeg ≥ 5.1 with `--enable-libx264 --enable-libx265
--enable-nvenc` (or your HW backend).

---

## 3. First boot

Default storage is JSON flat-file under `./test_data/`. To override
location or backend, set env vars BEFORE first boot:

```bash
export OPEN_STREAMER_STORAGE_DRIVER=yaml
export OPEN_STREAMER_STORAGE_YAML_DIR=/var/lib/open-streamer
./bin/open-streamer
```

The server starts unconfigured (no listeners, no streams) on first boot.
Configure via the REST API or UI. To enable HTTP API, POST a server
config:

```bash
curl -XPOST http://localhost:8080/api/v1/config -d '{
  "server":    { "http_addr": ":8080" },
  "buffer":    {},
  "manager":   {},
  "publisher": { "hls":  { "dir": "/var/hls"  },
                 "dash": { "dir": "/var/dash" } },
  "listeners": { "rtmp": { "enabled": true, "port": 1935 },
                 "rtsp": { "enabled": true, "port": 554  },
                 "srt":  { "enabled": true, "port": 9999 } }
}'
```

If the server isn't running on `:8080` yet, initialize via the YAML
endpoint or pre-seed `open_streamer.yaml` in your data dir.

---

## 4. Create a stream

Streams are the central entity — each binds N inputs (with priority
failover), an optional transcoder ladder, output protocols, push
destinations, and DVR settings. Stream code (`[a-zA-Z0-9_]`) is your
primary key.

### Minimum viable: pull HLS, publish HLS

```bash
curl -XPOST http://localhost:8080/api/v1/streams/news -d '{
  "name": "News Channel",
  "inputs": [
    { "url": "https://upstream.example.com/news/playlist.m3u8", "priority": 0 }
  ],
  "protocols": { "hls": true }
}'
```

Stream is up at `http://localhost:8080/news/index.m3u8`. No transcoding
— packets pass through untouched.

### Multi-input failover

Add backup sources sorted by priority (lower = preferred):

```bash
curl -XPOST http://localhost:8080/api/v1/streams/news -d '{
  "inputs": [
    { "url": "rtmp://primary.example.com/live/news",   "priority": 0 },
    { "url": "rtmp://backup.example.com/live/news",    "priority": 1 },
    { "url": "https://cdn.example.com/news/index.m3u8","priority": 2 }
  ],
  "protocols": { "hls": true }
}'
```

Stream Manager monitors the active input. On failure (configurable via
`manager.input_packet_timeout_sec`, default 30s) it switches to the
next-priority input within ~150ms — **without restarting FFmpeg**. The
switch is recorded in `runtime.switches[]`.

When the higher-priority input recovers (background probe succeeds), the
manager fails back automatically. Override manually:

```bash
curl -XPOST http://localhost:8080/api/v1/streams/news/inputs/switch \
     -d '{ "priority": 0 }'
```

### Add ABR transcoding

```bash
curl -XPOST http://localhost:8080/api/v1/streams/news -d '{
  "transcoder": {
    "global": { "hw": "nvenc" },
    "audio":  { "codec": "aac", "bitrate": 128 },
    "video": {
      "profiles": [
        { "width": 1920, "height": 1080, "bitrate": 5000, "codec": "h264" },
        { "width": 1280, "height": 720,  "bitrate": 2500, "codec": "h264" },
        { "width": 854,  "height": 480,  "bitrate": 1200, "codec": "h264" }
      ]
    }
  },
  "protocols": { "hls": true, "dash": true }
}'
```

Each profile becomes one FFmpeg process emitting an MPEG-TS stream into
its own buffer. HLS master playlist auto-aggregates them at
`/news/index.m3u8`. DASH MPD at `/news/index.mpd`.

**Multi-output mode** (single FFmpeg, N output pipes) cuts NVDEC + RAM
~50% per stream — toggle server-wide:

```bash
curl -XPOST http://localhost:8080/api/v1/config -d '{
  "transcoder": { "multi_output": true }
}'
```

Restart of running streams is automatic. Trade-off: 1 input glitch
brings down all profiles together (~2-3s) instead of just one
rendition.

### Push to platforms

```bash
curl -XPOST http://localhost:8080/api/v1/streams/news -d '{
  "push": [
    {
      "url": "rtmp://a.rtmp.youtube.com/live2/STREAM_KEY",
      "enabled": true
    },
    {
      "url": "rtmps://live-api-s.facebook.com:443/rtmp/STREAM_KEY",
      "enabled": true,
      "limit": 10,
      "retry_timeout_sec": 30
    }
  ]
}'
```

Per-destination state visible at `runtime.publisher.pushes[]` —
status (`starting` / `active` / `reconnecting` / `failed`), attempt
counter, last 5 errors, connected_at timestamp.

### DVR

Per-stream opt-in:

```bash
curl -XPOST http://localhost:8080/api/v1/streams/news -d '{
  "dvr": {
    "enabled":           true,
    "segment_duration":  4,
    "retention_sec":     604800,
    "max_size_gb":       100
  }
}'
```

Storage path defaults to `./out/dvr/{streamCode}`. Recording resumes
across restarts using the on-disk `playlist.m3u8` + `index.json`.
`#EXT-X-DISCONTINUITY` markers on every gap (signal loss + server
downtime).

VOD playback — full recording:
```
GET /recordings/news/playlist.m3u8
```

Timeshift — last 30 minutes:
```
GET /recordings/news/timeshift.m3u8?offset_sec=-1800&duration=1800
```

Timeshift — absolute window:
```
GET /recordings/news/timeshift.m3u8?from=2026-04-26T10:00:00Z&duration=3600
```

---

## 5. Hot-reload

`PUT /streams/{code}` (or repeat `POST`) merges only the changed fields:

```bash
# Add 360p rung — only ONE new FFmpeg process spawns; existing rungs untouched.
curl -XPOST http://localhost:8080/api/v1/streams/news -d '{
  "transcoder": {
    "video": {
      "profiles": [
        { "width": 1920, "height": 1080, "bitrate": 5000 },
        { "width": 1280, "height": 720,  "bitrate": 2500 },
        { "width": 854,  "height": 480,  "bitrate": 1200 },
        { "width": 640,  "height": 360,  "bitrate": 800  }
      ]
    }
  }
}'
```

The diff engine handles 5 categories independently — see
[ARCHITECTURE.md § Coordinator](./ARCHITECTURE.md#coordinator).

To force a full pipeline restart (e.g. switch stream key):

```bash
curl -XPOST http://localhost:8080/api/v1/streams/news/restart
```

To remove a stream entirely:

```bash
curl -XDELETE http://localhost:8080/api/v1/streams/news
```

---

## 6. Hooks (webhooks + file sink)

Subscribe to lifecycle events with one of two delivery backends:

```bash
# HTTP webhook — events ship as a JSON ARRAY (batched). HMAC signing
# covers the entire array body when `secret` is set.
curl -XPOST http://localhost:8080/api/v1/hooks -d '{
  "id":        "log-everything",
  "type":      "http",
  "target":    "https://ops.example.com/streams/events",
  "secret":    "shared-secret-for-hmac",
  "enabled":   true,
  "max_retries": 5,
  "timeout_sec": 10,

  // HTTP batching knobs — leave 0 to use server-wide defaults.
  "batch_max_items":          50,    // ship after every 50 events
  "batch_flush_interval_sec": 2,     // ... or every 2 seconds, whichever first
  "batch_max_queue_items":    20000  // memory cap if target goes down
}'

# File sink — appends one JSON event per line. NOT batched (one event per
# write) so log shippers (Filebeat / Vector / Promtail) tail-and-ship one
# line at a time.
curl -XPOST http://localhost:8080/api/v1/hooks -d '{
  "id":      "audit-log",
  "type":    "file",
  "target":  "/var/log/open-streamer/events.log",
  "enabled": true,
  "event_types": ["stream.started", "stream.stopped", "input.failover"]
}'
```

### HTTP batching semantics

- Each event is enqueued to a per-hook in-memory buffer (~µs latency).
- A flusher goroutine ships the buffer when EITHER `batch_max_items` is
  reached OR `batch_flush_interval_sec` elapses since the last flush.
- POST body is a JSON array (`[{event1},{event2},…]`). The
  `X-OpenStreamer-Batch-Size` header reports the array length so receivers
  can tune ingestion.
- `max_retries` retries within a single flush attempt (with 1s/5s/30s
  backoff). If all retries fail, events re-queue at the FRONT of the
  buffer for the next flush — chronological order preserved.
- `batch_max_queue_items` caps the buffer when the downstream is
  unreachable. Overflow drops the OLDEST events with a warning log.
- On graceful server shutdown (SIGTERM), every batcher gets one last
  best-effort flush.

### File backend behaviour

The file backend creates the target on first delivery (mode 0644). The
parent directory must already exist and be writable by the open-streamer
process. Concurrent deliveries on the same path serialise via a
per-target mutex; different paths run in parallel. Each line is one
complete JSON event followed by `\n` — the same envelope the HTTP
backend wraps in an array.

Filter by event type or stream code:

```json
{
  "event_types": ["input.failover", "transcoder.error"],
  "stream_codes": { "only": ["news", "sports"] }
}
```

Test delivery:

```bash
curl -XPOST http://localhost:8080/api/v1/hooks/log-everything/test
```

Hook server-wide settings (worker pool size) are at
`global_config.hooks` — see [CONFIG.md](./CONFIG.md). Per-hook
defaults: 3 retries, 10s timeout (overridden via Hook fields above).

For the full event catalogue see [APP_FLOW.md § Events
reference](./APP_FLOW.md#events-reference).

---

## 7. copy:// and mixer:// — in-process re-stream

Re-stream another in-process stream as input. No network round-trip; the
downstream stream subscribes directly to the upstream's published
buffer.

### copy:// — straight relay

```json
{
  "code": "news_backup",
  "inputs": [
    { "url": "copy://news", "priority": 0 }
  ],
  "protocols": { "hls": true }
}
```

If `news` has an ABR ladder, `news_backup` mirrors every rendition into
a parallel ABR ladder (no transcoding, just buffer subscriptions). This
is **ABR-copy** mode — the coordinator detects it and bypasses ingest +
transcoder entirely.

If `news` has no ladder, `copy://news` subscribes to its main playback
buffer — single-rendition copy.

Cycle detection prevents `copy://A` ↔ `copy://B` infinite loops via the
copy-graph validator at save time.

### mixer:// — combine video + audio from two streams

```json
{
  "code": "tv_with_radio",
  "inputs": [
    { "url": "mixer://tv_silent?audio=radio_fm", "priority": 0 }
  ],
  "protocols": { "hls": true }
}
```

Video tracks come from `tv_silent`, audio tracks from `radio_fm`.
Useful for muting a TV broadcast and overlaying a radio commentary.

Both streams must already exist; mixer:// validates at save time
(`audio=` param required, no self-mix, no nested mixer chains).

---

## 8. Watermarks

Apply text or image overlays to the encoded video. Two-step workflow
for image watermarks: upload to the asset library, then reference by
asset ID from a stream.

### 8.1 Upload an image asset

```bash
curl -XPOST http://localhost:8080/api/v1/watermarks?name=ChannelLogo \
     -F "file=@channel-logo.png"
# 201 Created
# {
#   "data": {
#     "id":          "8a3f1c0e2b9d",
#     "name":        "ChannelLogo",
#     "file_name":   "channel-logo.png",
#     "content_type":"image/png",
#     "size_bytes":  4521,
#     "uploaded_at": "2026-04-28T15:00:00Z"
#   }
# }
```

PNG / JPG / GIF supported. Cap: 8 MiB per asset. Library stays under
`watermarks.dir` (default `./watermarks`).

```bash
curl http://localhost:8080/api/v1/watermarks | jq .         # list
curl http://localhost:8080/api/v1/watermarks/8a3f1c0e2b9d/raw -o /tmp/preview.png
curl -XDELETE http://localhost:8080/api/v1/watermarks/8a3f1c0e2b9d
```

### 8.2 Apply to a stream

**Image watermark** referencing the uploaded asset:

```bash
curl -XPOST http://localhost:8080/api/v1/streams/news -d '{
  "watermark": {
    "enabled":   true,
    "type":      "image",
    "asset_id":  "8a3f1c0e2b9d",
    "position":  "top_right",
    "offset_x":  30, "offset_y": 30,
    "opacity":   0.85
  }
}'
```

**Text watermark** with a live clock:

```bash
curl -XPOST http://localhost:8080/api/v1/streams/news -d '{
  "watermark": {
    "enabled":    true,
    "type":       "text",
    "text":       "LIVE %{localtime\\:%H\\:%M}",
    "font_size":  28,
    "font_color": "white",
    "opacity":    0.9,
    "position":   "bottom_right",
    "offset_x":   20, "offset_y": 20
  }
}'
```

**Custom position** (raw FFmpeg expression — full power):

```bash
curl -XPOST http://localhost:8080/api/v1/streams/news -d '{
  "watermark": {
    "enabled":  true,
    "type":     "image",
    "asset_id": "8a3f1c0e2b9d",
    "position": "custom",
    "x":        "main_w-overlay_w-50",
    "y":        "if(gt(t,5),10,-100)"
  }
}'
```

The `y` example slides the watermark in from above the frame after 5
seconds — useful for animated brand intros without an external editor.

### 8.3 Position presets

| Position | Where | offset_x / offset_y mean |
|---|---|---|
| `top_left` | upper-left corner | inward padding from edge |
| `top_right` | upper-right corner | inward padding |
| `bottom_left` | lower-left corner | inward padding |
| `bottom_right` | lower-right corner (default) | inward padding |
| `center` | exact frame centre | offsets ignored |
| `custom` | wherever your `x` / `y` expression evaluates | offsets ignored |

GPU pipelines (NVENC) automatically round-trip via CPU for the
watermark filter — costs ~5% CPU per FFmpeg process; no operator
action needed. Multi-output mode applies the watermark per rendition
independently.

### 8.4 Updating watermark on a running stream

Changing any watermark field on a stream that is currently transcoding
**restarts the transcoder pipeline** (~2-3s downtime per stream — same
shape as toggling `transcoder.multi_output`):

- Buffer hub keeps running; rendition buffers are recreated on the new
  start path
- HLS / DASH / RTMP / SRT / RTSP viewers see one
  `#EXT-X-DISCONTINUITY` (HLS) or equivalent gap, then resume
- DVR pauses for the gap and adds a `DVRGap` entry

This is necessary because the FFmpeg `-vf` filter chain is baked into
the encoder's argv at spawn time — there's no way to live-swap it
without restarting the process. The diff engine routes any
watermark-related field change through the topology-reload path so
both legacy and multi-output modes pick up the new filter graph
uniformly.

**Watermark on a passthrough stream** (no transcoder) is silently
ignored — no FFmpeg is running so the filter graph never exists.
Enable transcoding to get a server-side watermark; otherwise overlay
client-side in your player.

---

## 9. Play sessions — who's watching?

Open Streamer tracks every active player across all delivery
protocols (HLS / DASH / RTMP / SRT / RTSP). State is in-memory only —
restart loses records, viewers reconnect into fresh sessions.

### 9.1 Enable

Sessions tracking is **opt-in** — POST a `sessions` config section:

```bash
curl -XPOST http://localhost:8080/api/v1/config -d '{
  "sessions": { "enabled": true, "idle_timeout_sec": 30 }
}'
```

Hot-reloadable: toggling `enabled` or changing `idle_timeout_sec`
takes effect on the next reaper tick (≤ 5s) without restart.

### 9.2 Inspect

```bash
# All active sessions
curl http://localhost:8080/api/v1/sessions | jq .

# Per-stream
curl http://localhost:8080/api/v1/streams/news/sessions | jq .data

# Filter by protocol
curl 'http://localhost:8080/api/v1/sessions?proto=hls' | jq .data
```

Each session record includes:

```json
{
  "id":          "abc123…",                       // fingerprint or UUID
  "stream_code": "news",
  "proto":       "hls",                           // hls|dash|rtmp|srt|rtsp
  "ip":          "203.0.113.42",
  "user_agent":  "Mozilla/5.0 …",
  "country":     "VN",                            // when GeoIP wired
  "bytes":       1481726,
  "opened_at":   "2026-04-28T15:01:00Z",
  "updated_at":  "2026-04-28T15:04:13Z",
  "duration_sec": 193
}
```

The list response also includes aggregate `stats` (active /
opened_total / closed_total / idle_closed_total / kicked_total).

### 9.3 Kick a viewer

```bash
curl -XDELETE http://localhost:8080/api/v1/sessions/abc123…
# 204 No Content
```

Idempotent — calling delete on an already-closed session returns 404.
Emits `EventSessionClosed` with `reason=kicked` on the event bus —
hooks subscribed to the event see the kick in the same channel as
organic disconnects.

### 9.4 Subscribe to session events

```bash
curl -XPOST http://localhost:8080/api/v1/hooks -d '{
  "id":     "viewer-analytics",
  "type":   "file",
  "target": "/var/log/open-streamer/sessions.log",
  "event_types": ["session.opened", "session.closed"]
}'
```

The bus carries an event for every open/close — feed Loki, ClickHouse,
or your analytics pipeline. The sessions package stays in-memory; long-
term persistence is the hook's responsibility.

### 9.5 Notes & caveats

- **Fingerprint sessions** (HLS / DASH) collapse repeated GETs from one
  viewer onto one record while the idle window is open. Two viewers
  behind shared NAT without a `?token=…` will merge into one session —
  add a token query param to disambiguate (the tracker stores it as
  the user_name and `named_by="token"`).
- **RTMP / SRT / RTSP** are connection-bound — one TCP/UDP session per
  record, closed exactly when the transport ends.
- **RTSP bytes** are always 0: gortsplib's mux is internal and there's
  no per-subscriber hook today. Other counters are accurate.
- **GeoIP** field is empty unless an operator wires a custom resolver —
  the default `NullGeoIP` always returns `""`. The `geoip_db_path`
  config field is reserved for the future MaxMind integration.

---

## 10. Operations

### Health checks

- `GET /healthz` — liveness (always 200 if process is up)
- `GET /readyz` — readiness (200 once services initialised)

### Metrics

Prometheus scrape endpoint at `GET /metrics`:

- `manager_failovers_total{stream_code}` — rate of input switches
- `transcoder_restarts_total{stream_code}` — FFmpeg crash count
- `transcoder_workers_active{stream_code}` — running profile count
- `manager_input_health{stream_code, input_priority}` — 1 healthy, 0 degraded
- Buffer depth, bytes/packets per stream

### Per-stream runtime status

```bash
curl http://localhost:8080/api/v1/streams/news | jq .data.runtime
```

Returns:
- `status` — `active` / `degraded` / `stopped` / `idle`
- `pipeline_active` — bool
- `active_input_priority` — current source
- `exhausted` — true when all inputs degraded
- `inputs[]` — per-input health + last 5 errors
- `switches[]` — last 20 active-input switches with reason
- `transcoder.profiles[]` — per-rung restart count + errors
- `publisher.pushes[]` — per-destination state

A stream is `degraded` when EITHER:
- All inputs exhausted (manager: no failover candidate), OR
- Transcoder is in a crash loop (3 consecutive FFmpeg crashes < 30s
  apart). Recovers automatically when a process runs > 30s sustained.

### YAML editor (entire system state)

```bash
curl http://localhost:8080/api/v1/config/yaml > backup.yaml
# edit backup.yaml...
curl -XPUT http://localhost:8080/api/v1/config/yaml \
     -H "Content-Type: application/yaml" \
     --data-binary @backup.yaml
```

Round-trips GlobalConfig + all streams + all hooks. Useful for
ops-as-code workflows.

### Logs

`slog` structured output to stderr. Configurable via `log.level` (debug
/ info / warn / error) and `log.format` (text / json). Common ops grep:

```bash
journalctl -u open-streamer | grep -E '(failover|crashed|degraded)'
```

---

## 11. Troubleshooting

| Symptom | Likely cause | Where to look |
|---|---|---|
| Status `degraded` while inputs healthy | Transcoder is in a crash loop on at least one rung | `runtime.transcoder.profiles[].errors[]` (UI or `/streams/{code}`) |
| FFmpeg rejects an encoder option | Codec/preset typo, or option unsupported by the resolved encoder build | Stream config + Settings → Probe FFmpeg checklist |
| Stream up but HLS 404 | `publisher.hls.dir` is empty, or transcoder not producing output | Server logs (`publisher: HLS disabled — …` etc.) |
| Push stuck in `reconnecting` | Destination reject, auth fail, or network loss | `runtime.publisher.pushes[].errors[]` |
| GPU encoder near 100% saturation | NVENC chip overloaded — reduce profile count / framerate, set `bframes=0`, or enable `transcoder.multi_output` | `nvidia-smi` or Grafana GPU dashboard |
| `copy://X` save rejected | `X` doesn't exist, OR cycle detected, OR shape constraint violated | API error message details which check failed |
| Boot fails with "ffmpeg incompatible" | A required encoder/muxer is missing from the FFmpeg build | Probe response under `errors[]`; rebuild ffmpeg with `--enable-libx264` etc. |
| `/sessions` returns empty after enabling | Tracker config not persisted (UI didn't save) OR sub-section absent before restart | `curl /api/v1/config \| jq .sessions` to verify; restart once after first enabling |
| Watermark POST returns `INVALID_WATERMARK` | `image_path` and `asset_id` both set, OR custom position with empty x/y, OR opacity outside [0,1] | API error message names the rule; fix the config JSON |
| Watermark image not appearing on output | Asset deleted while stream running, OR `image_path` not absolute / unreadable by the open-streamer user | Server logs (`coordinator: watermark asset resolve failed`); chown the asset / re-upload |

For deeper troubleshooting see [APP_FLOW.md](./APP_FLOW.md) — covers
exact event sequences, status reconciliation logic, and what each error
in the runtime snapshot means.

---

## 12. Production checklist

- [ ] FFmpeg installed with required encoders (boot probe will catch this)
- [ ] HLS + DASH dirs are different (when both enabled)
- [ ] HTTP server bind address chosen — reverse proxy in front for TLS
- [ ] Storage backend chosen (`json` flat-file or `yaml` single-doc)
- [ ] Hooks configured for at least `stream.stopped` + `transcoder.error` so ops gets paged on crashes
- [ ] DVR retention sized against disk capacity
- [ ] `manager.input_packet_timeout_sec` tuned per-protocol (HLS pull bursts may need ≥ segment duration × 2)
- [ ] Prometheus scrape configured against `/metrics`
- [ ] Pre-commit hook installed for contributors (`make hooks-install`)

---

## 13. Updating

```bash
# Use the provided installer for atomic upgrades on systemd hosts:
sudo bash build/reinstall.sh v1.0.1
```

Stops the service, swaps the binary + systemd unit, restarts. Data
directory (streams, recordings, hooks) is preserved across upgrades.

For source builds: `git pull && make build && systemctl restart
open-streamer`.

---

## See also

- [CONFIG.md](./CONFIG.md) — every config field explained, with examples
- [ARCHITECTURE.md](./ARCHITECTURE.md) — design rationale, data flow,
  invariants
- [APP_FLOW.md](./APP_FLOW.md) — pipeline lifecycle, event sequences,
  status reconciliation
- [FEATURES_CHECKLIST.md](./FEATURES_CHECKLIST.md) — what's
  implemented vs planned
