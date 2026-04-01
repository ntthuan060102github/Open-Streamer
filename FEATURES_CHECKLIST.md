# Open-Streamer — Feature checklist

Legend for **Completion**:

| Level | Meaning |
|--------|---------|
| **Complete** | Implemented and usable for the described scope |
| **Partial** | Works with known limitations, TODOs, or narrow codec/path support |
| **Stub** | Registered in config/API but no real implementation (placeholder) |
| **Schema only** | Domain / API / persistence fields exist; not wired into ingest/transcode/publish |
| **Planned** | Documented intent only |

---

## Core platform

| Feature | Completion | Notes |
|---------|------------|--------|
| Configuration (`config`, viper/env) | Complete | Single `Config` loaded at startup |
| Dependency injection (`samber/do`) | Complete | `cmd/server` wires services |
| Structured logging (`slog`, `pkg/logger`) | Complete | |
| Graceful shutdown (SIGINT/SIGTERM) | Complete | |
| Prometheus metrics (`internal/metrics`) | Partial | Module present; extent of coverage depends on registered collectors |

---

## Storage & API

| Feature | Completion | Notes |
|---------|------------|--------|
| Stream repository (JSON file) | Complete | Default store under configurable JSON dir |
| Stream repository (SQL) | Partial | Package exists; wiring depends on deployment config |
| Stream repository (MongoDB) | Partial | Package exists; wiring depends on deployment config |
| Recording repository | Partial | Same as store driver; DVR disk I/O not finished |
| Hook repository | Complete | With JSON (and other drivers if configured) |
| REST API — streams CRUD / start / stop / status | Complete | Chi router under `/streams` |
| REST API — recordings (start/stop/list/get/delete) | Partial | API works; recording **segment files + VOD playlist** not written to disk yet |
| REST API — hooks CRUD + test (HTTP) | Partial | HTTP hook test supported; NATS/Kafka test paths limited |
| OpenAPI / Swagger | Complete | Generated via `swag`; `make swagger` |
| HTTP static delivery — HLS master + nested paths | Complete | `/{code}/index.m3u8`, `/{code}/*` for `track_N/…` |
| HTTP static delivery — DASH root + nested paths | Complete | `/{code}/index.mpd`, `/{code}/*` for shards |
| Health / readiness | Complete | `/healthz`, `/readyz` |

---

## Buffer Hub

| Feature | Completion | Notes |
|---------|------------|--------|
| In-memory ring buffer per stream id | Complete | Fan-out via subscribers |
| Raw ingest buffer (`$raw$<code>`) | Complete | Used when internal transcoding is active |
| Rendition buffers (`$r$<code>$track_N`) | Complete | One buffer per ladder rung |
| Playback buffer selection (best track) | Complete | `PlaybackBufferID` for single-rendition outputs |

---

## Ingest

| Feature | Completion | Notes |
|---------|------------|--------|
| Pull — HLS | Complete | |
| Pull — HTTP | Complete | |
| Pull — RTSP | Complete | |
| Pull — RTMP | Complete | |
| Pull — SRT (caller) | Complete | |
| Pull — UDP / MPEG-TS | Partial | Multicast auto-join TODO in `pull/udp.go` |
| Pull — file | Complete | |
| Pull — S3 | Complete | |
| Push — RTMP listen | Complete | Stream key → registry → buffer |
| Push — SRT listen | Complete | streamid pattern → registry |
| Ingest → configurable write target | Complete | Main code or `$raw$…` when transcoding |

---

## Stream manager (failover)

| Feature | Completion | Notes |
|---------|------------|--------|
| Multi-input registration with priority | Complete | |
| Health / timeout / degraded detection | Complete | |
| Failover between inputs (Go-level) | Complete | No FFmpeg restart for switch |
| Events (degraded, failover, etc.) | Complete | Published to event bus |

---

## Transcoder

| Feature | Completion | Notes |
|---------|------------|--------|
| Internal FFmpeg transcode (stdin TS → stdout TS) | Complete | libx264 + AAC by default; HW mapping for NVENC etc. |
| Multiple profiles = multiple FFmpeg processes | Complete | One encoder per `track_N`; shared raw input |
| Worker pool / max concurrent encoders | Complete | Semaphore in `transcoder.Service` |
| Video ladder config (`VideoProfile` by order) | Complete | Stable ids: `track_1`, `track_2`, … (no per-profile name field) |
| Copy video/audio modes | Complete | Via `TranscoderConfig` |
| Extra FFmpeg args | Complete | `ExtraArgs` passthrough |
| Passthrough / remux “modes” in domain | Schema only | Domain has concepts; pipeline is encode-focused for ladder |
| Log noise control (timestamp discontinuity) | Complete | Demoted to debug when ingest timeline jumps (e.g. failover) |

---

## Publisher — delivery

| Feature | Completion | Notes |
|---------|------------|--------|
| HLS — single rendition | Complete | Native TS segmenter + media playlist |
| HLS — ABR (master + `track_N` sub-playlists) | Complete | When internal transcoding ladder is active |
| DASH — single representation (fMP4 + dynamic MPD) | Partial | H.264 + AAC from TS; **H.265 in TS ignored** with warning in packager |
| DASH — ABR (root MPD + per-track directories) | Complete | Audio packaged only on **best** track folder |
| RTSP (MPEG-TS in RTP, gortsplib) | Complete | Shared listener; `/live/<code>` |
| RTMP play (gomedia) | Complete | Shared listener; app `live` |
| SRT listen (gosrt) | Complete | `streamid=live/<code>` pattern |
| RTMP push out | Partial | **rtmp://** supported; other schemes error clearly |
| RTS / WebRTC (WHEP) | Stub | Logs not implemented; suggests HLS/DASH for browsers |

---

## Coordinator & lifecycle

| Feature | Completion | Notes |
|---------|------------|--------|
| Start pipeline (buffer, manager, publisher, transcoder) | Complete | Creates raw + rendition buffers as needed |
| Stop pipeline / teardown buffers | Complete | Including `$r$…` slugs |
| Bootstrap persisted streams on startup | Complete | Skips stopped / no-input streams |

---

## DVR

| Feature | Completion | Notes |
|---------|------------|--------|
| Start/stop recording session (API + events) | Partial | Subscribes from `PlaybackBufferID` |
| Segment splitting by duration | Partial | In-memory path; **no disk write / no M3U8 file** (`flushSegment` TODO) |
| Recording metadata in store | Partial | Depends on `Save` path; on-disk artifacts missing |

---

## Events & hooks

| Feature | Completion | Notes |
|---------|------------|--------|
| In-process event bus | Complete | Typed events; worker pool |
| HTTP webhook delivery | Complete | Retries, timeouts, optional HMAC |
| NATS delivery | Stub | Returns “not implemented” |
| Kafka delivery | Stub | Returns “not implemented” |

---

## Domain extras (not in live pipeline)

| Feature | Completion | Notes |
|---------|------------|--------|
| Watermark config on `Stream` | Schema only | Not applied in transcoder graph |
| Thumbnail config on `Stream` | Schema only | Not generated alongside outputs |

---

## Testing & quality

| Feature | Completion | Notes |
|---------|------------|--------|
| Go unit tests (selected packages) | Partial | e.g. ingestor, publisher, transcoder, protocol — run `go test ./...` |
| CI / lint | Partial | `.golangci.yml` present; ensure CI runs in your fork |

---

## Operational assumptions

- **FFmpeg** must be available where `transcoder` expects (`FFmpegPath` in config).
- **HLS/DASH dirs** must differ when both are enabled.
- **Failover** between unrelated HLS origins may produce FFmpeg timestamp discontinuities (handled in logs; optional debug for detail).

---

*Last reviewed against repository layout and code paths (internal packages). Update this file when features move between columns.*
