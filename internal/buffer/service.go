// Package buffer implements the Buffer Hub — the central in-memory ring buffer.
// It is the single data pipeline between Ingestor and all consumers (Transcoder, Publisher, DVR).
// Each stream has its own ring buffer; consumers subscribe and get an independent read cursor.
package buffer

import (
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/do/v2"

	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics"
)

// Subscriber is a read cursor into a stream's ring buffer.
type Subscriber struct {
	ch chan Packet
}

// Recv returns the channel from which the subscriber reads packets.
func (s *Subscriber) Recv() <-chan Packet { return s.ch }

// ringBuffer is a bounded in-memory queue for a single stream.
//
// `drops` is a pre-bound Prometheus counter for this stream's drop events
// — pre-binding once (instead of `WithLabelValues` per write) keeps the
// hot path map-lookup-free. Nil when metrics aren't injected (tests).
type ringBuffer struct {
	mu    sync.Mutex
	subs  []*Subscriber
	drops prometheus.Counter
}

func (rb *ringBuffer) write(pkt Packet) {
	if pkt.empty() {
		return
	}
	// Hold the lock during fan-out so a concurrent unsubscribe can't close
	// a subscriber channel while we're sending to it. Sends use `select default`
	// so the writer still never blocks on slow consumers — the dropped packet
	// is counted into rb.drops so operators can see slow-consumer pressure
	// before users complain about frame drops.
	rb.mu.Lock()
	defer rb.mu.Unlock()
	for _, s := range rb.subs {
		pc := clonePacket(pkt)
		select {
		case s.ch <- pc:
		default:
			if rb.drops != nil {
				rb.drops.Inc()
			}
		}
	}
}

func (rb *ringBuffer) subscribe(chanSize int) *Subscriber {
	s := &Subscriber{ch: make(chan Packet, chanSize)}
	rb.mu.Lock()
	rb.subs = append(rb.subs, s)
	rb.mu.Unlock()
	return s
}

func (rb *ringBuffer) unsubscribe(s *Subscriber) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	for i, sub := range rb.subs {
		if sub == s {
			rb.subs = append(rb.subs[:i], rb.subs[i+1:]...)
			close(s.ch)
			return
		}
	}
}

// unsubscribeAll closes every subscriber's channel and clears the list.
// Used by Service.UnsubscribeAll to signal "no more data from this buffer"
// to all live consumers (e.g. when a downstream needs to detect upstream
// teardown via channel close rather than indefinite blocking).
func (rb *ringBuffer) unsubscribeAll() {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	for _, s := range rb.subs {
		close(s.ch)
	}
	rb.subs = nil
}

// Service manages ring buffers for all active streams.
type Service struct {
	cfg     config.BufferConfig
	m       *metrics.Metrics
	mu      sync.RWMutex
	buffers map[domain.StreamCode]*ringBuffer
}

// New creates a Service and registers it with the DI injector.
func New(i do.Injector) (*Service, error) {
	cfg := do.MustInvoke[config.BufferConfig](i)
	if cfg.Capacity <= 0 {
		cfg.Capacity = domain.DefaultBufferCapacity
	}
	// Metrics is registered as a separate provider; tolerate absence so
	// tests that wire only buffer can still construct the service.
	var m *metrics.Metrics
	if mm, err := do.Invoke[*metrics.Metrics](i); err == nil {
		m = mm
	}
	svc := &Service{
		cfg:     cfg,
		m:       m,
		buffers: make(map[domain.StreamCode]*ringBuffer),
	}
	if m != nil {
		// Sampler runs forever — it's cheap (constant-time per stream) and
		// has no graceful-stop contract because the process owns its own
		// lifecycle. 2s is fast enough for the 15s default Prometheus
		// scrape interval to see fresh-ish values without overlap, while
		// being slow enough that the sampler doesn't show up in CPU
		// profiles for production-sized stream counts (~hundreds).
		go svc.sampleLoop(2 * time.Second)
	}
	return svc, nil
}

// sampleLoop periodically computes BufferCapacityUsed (max channel depth
// across subscribers, normalised to 0..1) and BufferSubscribers (count) per
// stream and writes them to the gauges. Reads are RLock-only — no impact
// on the write hot path.
func (s *Service) sampleLoop(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		s.sampleOnce()
	}
}

// sampleOnce takes one snapshot. Split out so a future graceful-shutdown
// hook can call it once on the way out for a final-tick refresh, and so
// tests can drive the metrics deterministically without relying on time.
func (s *Service) sampleOnce() {
	if s.m == nil {
		return
	}
	type bufSnap struct {
		code     domain.StreamCode
		maxDepth int
		nSubs    int
		cap      int
	}
	s.mu.RLock()
	snaps := make([]bufSnap, 0, len(s.buffers))
	for code, rb := range s.buffers {
		rb.mu.Lock()
		max := 0
		for _, sub := range rb.subs {
			if d := len(sub.ch); d > max {
				max = d
			}
		}
		snaps = append(snaps, bufSnap{code: code, maxDepth: max, nSubs: len(rb.subs), cap: s.cfg.Capacity})
		rb.mu.Unlock()
	}
	s.mu.RUnlock()
	for _, sn := range snaps {
		var frac float64
		if sn.cap > 0 {
			frac = float64(sn.maxDepth) / float64(sn.cap)
		}
		s.m.BufferCapacityUsed.WithLabelValues(string(sn.code)).Set(frac)
		s.m.BufferSubscribers.WithLabelValues(string(sn.code)).Set(float64(sn.nSubs))
	}
}

// NewServiceForTesting creates a Service without DI, for use in unit tests.
func NewServiceForTesting(capacity int) *Service {
	return &Service{
		cfg:     config.BufferConfig{Capacity: capacity},
		buffers: make(map[domain.StreamCode]*ringBuffer),
	}
}

// Create initialises a ring buffer for the given stream.
func (s *Service) Create(id domain.StreamCode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.buffers[id]; !ok {
		rb := &ringBuffer{}
		if s.m != nil {
			rb.drops = s.m.BufferDropsTotal.WithLabelValues(string(id))
		}
		s.buffers[id] = rb
	}
}

// Write pushes a packet into the stream's ring buffer (deep-copied for subscribers).
// Only the active Ingestor goroutine for this stream should call Write.
func (s *Service) Write(id domain.StreamCode, pkt Packet) error {
	s.mu.RLock()
	rb, ok := s.buffers[id]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("buffer: stream %s not found", id)
	}
	rb.write(pkt)
	return nil
}

// Subscribe registers a new consumer for the stream's buffer.
// The caller must call Unsubscribe when done to avoid a goroutine/channel leak.
func (s *Service) Subscribe(id domain.StreamCode) (*Subscriber, error) {
	s.mu.RLock()
	rb, ok := s.buffers[id]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("buffer: stream %s not found", id)
	}
	return rb.subscribe(s.cfg.Capacity), nil
}

// Unsubscribe removes a consumer and closes its channel.
func (s *Service) Unsubscribe(id domain.StreamCode, sub *Subscriber) {
	s.mu.RLock()
	rb, ok := s.buffers[id]
	s.mu.RUnlock()
	if ok {
		rb.unsubscribe(sub)
	}
}

// Delete tears down the ring buffer for a stream (call when stream is stopped).
// Closes every subscriber's channel BEFORE removing the map entry so consumers
// observe a clean EOF (`pkt, ok := <-sub.Recv(); !ok`) and can react — the
// mixer / copy taps in particular rely on this signal to retry against the
// freshly-created buffer when an upstream stream restarts. Without the
// channel-close, taps stay blocked on a stale ringBuffer indefinitely while
// the new ringBuffer for the same id receives packets they never see.
func (s *Service) Delete(id domain.StreamCode) {
	s.mu.Lock()
	rb, ok := s.buffers[id]
	if ok {
		delete(s.buffers, id)
	}
	s.mu.Unlock()
	if ok {
		rb.unsubscribeAll()
	}
}

// UnsubscribeAll closes every subscriber's channel for the given buffer.
// Subscribers' next Recv will see ok=false (a clean EOF signal). The buffer
// itself is left in place — call Delete after if you also want it removed.
// No-op when the buffer doesn't exist.
func (s *Service) UnsubscribeAll(id domain.StreamCode) {
	s.mu.RLock()
	rb, ok := s.buffers[id]
	s.mu.RUnlock()
	if !ok {
		return
	}
	rb.unsubscribeAll()
}
