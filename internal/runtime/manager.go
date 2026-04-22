// Package runtime manages the lifecycle of all long-running services based on
// the persisted GlobalConfig.  Each service is independently startable and
// stoppable; the Manager diffs old vs new config and applies changes surgically.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"

	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/api"
	"github.com/ntt0601zcoder/open-streamer/internal/coordinator"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/events"
	"github.com/ntt0601zcoder/open-streamer/internal/hooks"
	"github.com/ntt0601zcoder/open-streamer/internal/ingestor"
	"github.com/ntt0601zcoder/open-streamer/internal/publisher"
	"github.com/ntt0601zcoder/open-streamer/internal/store"
	"github.com/ntt0601zcoder/open-streamer/pkg/logger"
)

// serviceEntry tracks one running service goroutine.
type serviceEntry struct {
	cancel context.CancelFunc
	done   chan struct{} // closed when the goroutine exits
}

// Deps holds all service references needed by the Manager.
// Populated by main.go from the DI injector.
type Deps struct {
	Ingestor         *ingestor.Service
	Publisher        *publisher.Service
	Coordinator      *coordinator.Coordinator
	HooksSvc         *hooks.Service
	APISrv           *api.Server
	Bus              events.Bus
	StreamRepo       store.StreamRepository
	GlobalConfigRepo store.GlobalConfigRepository
}

// Manager owns the lifecycle of all long-running services.
// It reads GlobalConfig from the store and starts/stops services accordingly.
type Manager struct {
	deps Deps

	mu       sync.Mutex
	services map[string]*serviceEntry
	current  *domain.GlobalConfig

	// rootCtx is the top-level application context (cancelled on SIGINT/SIGTERM).
	rootCtx context.Context
}

// New creates a Manager.  Call Bootstrap() to load config and start services.
func New(ctx context.Context, deps Deps) *Manager {
	return &Manager{
		deps:     deps,
		services: make(map[string]*serviceEntry),
		rootCtx:  ctx,
	}
}

// Bootstrap loads GlobalConfig from the store (seeding defaults on first boot),
// then starts all configured services.
func (m *Manager) Bootstrap() error {
	gcfg, err := m.loadOrSeed()
	if err != nil {
		return fmt.Errorf("runtime: bootstrap: %w", err)
	}

	m.mu.Lock()
	m.current = gcfg
	m.mu.Unlock()

	m.applyAll(gcfg)
	return nil
}

// BootstrapWith starts all configured services using the provided GlobalConfig.
// Use this when main.go has already loaded/seeded the config from the store.
func (m *Manager) BootstrapWith(gcfg *domain.GlobalConfig) {
	m.mu.Lock()
	m.current = gcfg
	m.mu.Unlock()

	m.applyAll(gcfg)
}

// CurrentConfig returns a copy of the active GlobalConfig.
func (m *Manager) CurrentConfig() *domain.GlobalConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// Apply saves a new GlobalConfig to the store and diffs against the current
// config to start/stop services as needed.
func (m *Manager) Apply(ctx context.Context, newCfg *domain.GlobalConfig) error {
	if err := m.deps.GlobalConfigRepo.Set(ctx, newCfg); err != nil {
		return fmt.Errorf("runtime: save config: %w", err)
	}

	m.mu.Lock()
	old := m.current
	m.current = newCfg
	m.mu.Unlock()

	m.diff(old, newCfg) //nolint:contextcheck // services derive context from rootCtx, not the request ctx
	return nil
}

// WaitAll blocks until all running services have exited.
func (m *Manager) WaitAll() {
	m.mu.Lock()
	entries := make([]*serviceEntry, 0, len(m.services))
	for _, e := range m.services {
		entries = append(entries, e)
	}
	m.mu.Unlock()
	for _, e := range entries {
		<-e.done
	}
}

// --- internal ---

func (m *Manager) loadOrSeed() (*domain.GlobalConfig, error) {
	gcfg, err := m.deps.GlobalConfigRepo.Get(context.Background())
	if err == nil {
		slog.Info("runtime: loaded global config from store")
		return gcfg, nil
	}
	if errors.Is(err, store.ErrNotFound) {
		slog.Info("runtime: no global config in store, starting unconfigured")
		return &domain.GlobalConfig{}, nil
	}
	return nil, fmt.Errorf("read stored config: %w", err)
}

// applyAll starts all services that are configured (initial boot).
func (m *Manager) applyAll(cfg *domain.GlobalConfig) {
	if cfg.Server != nil {
		m.startService("http", func(ctx context.Context) error {
			return m.deps.APISrv.StartWithConfig(ctx, cfg.Server)
		})
	}

	// Ingestor owns the shared RTMP listener. Even when no other ingestor
	// settings are configured, having listeners.rtmp.enabled is reason enough
	// to start it so external play clients work too.
	if cfg.Ingestor != nil || rtmpListenerEnabled(cfg.Listeners) {
		m.startService("ingestor", func(ctx context.Context) error {
			return m.deps.Ingestor.Run(ctx)
		})
	}

	if rtspListenerEnabled(cfg.Listeners) {
		m.startService("pub_rtsp", func(ctx context.Context) error {
			return m.deps.Publisher.RunRTSPPlayServer(ctx)
		})
	}
	if srtListenerEnabled(cfg.Listeners) {
		m.startService("pub_srt", func(ctx context.Context) error {
			return m.deps.Publisher.RunSRTPlayServer(ctx)
		})
	}

	if cfg.Hooks != nil {
		m.startService("hooks", func(ctx context.Context) error {
			return m.deps.HooksSvc.Start(ctx)
		})
	}

	// Event bus is always running (lightweight).
	events.Start(m.rootCtx, m.deps.Bus)

	// Wire ingestor → publisher RTMP play handler so the shared listener can
	// serve external play clients in addition to push ingest.
	m.deps.Ingestor.SetRTMPPlayHandler(m.deps.Publisher.HandleRTMPPlay)

	// Bootstrap persisted streams.
	coordinator.BootstrapPersistedStreams(
		m.rootCtx, slog.Default(), m.deps.StreamRepo, m.deps.Coordinator,
	)
}

// diff compares old and new configs and starts/stops services as needed.
func (m *Manager) diff(old, new *domain.GlobalConfig) {
	// HTTP server
	m.diffService("http", old.Server != nil, new.Server != nil,
		configChanged(old.Server, new.Server),
		func(ctx context.Context) error {
			return m.deps.APISrv.StartWithConfig(ctx, new.Server)
		})

	// Ingestor — owns the shared RTMP listener. Restart on changes to either
	// IngestorConfig or listeners.RTMP, and treat the listener being enabled
	// as sufficient reason to keep the service running even when IngestorConfig
	// is nil.
	wasIng := old.Ingestor != nil || rtmpListenerEnabled(old.Listeners)
	nowIng := new.Ingestor != nil || rtmpListenerEnabled(new.Listeners)
	ingChanged := configChanged(old.Ingestor, new.Ingestor) ||
		configChanged(rtmpListenerOf(old.Listeners), rtmpListenerOf(new.Listeners))
	m.diffService("ingestor", wasIng, nowIng, ingChanged,
		func(ctx context.Context) error {
			return m.deps.Ingestor.Run(ctx)
		})

	// Publisher RTSP listener
	oldRTSP := rtspListenerEnabled(old.Listeners)
	newRTSP := rtspListenerEnabled(new.Listeners)
	m.diffService("pub_rtsp", oldRTSP, newRTSP,
		configChanged(rtspListenerOf(old.Listeners), rtspListenerOf(new.Listeners)),
		func(ctx context.Context) error {
			return m.deps.Publisher.RunRTSPPlayServer(ctx)
		})

	// Publisher SRT listener
	oldSRT := srtListenerEnabled(old.Listeners)
	newSRT := srtListenerEnabled(new.Listeners)
	m.diffService("pub_srt", oldSRT, newSRT,
		configChanged(srtListenerOf(old.Listeners), srtListenerOf(new.Listeners)),
		func(ctx context.Context) error {
			return m.deps.Publisher.RunSRTPlayServer(ctx)
		})

	// Hooks
	m.diffService("hooks", old.Hooks != nil, new.Hooks != nil,
		configChanged(old.Hooks, new.Hooks),
		func(ctx context.Context) error {
			return m.deps.HooksSvc.Start(ctx)
		})

	// Log — not a long-running service, just swap the global logger.
	if configChanged(old.Log, new.Log) {
		if new.Log != nil {
			slog.SetDefault(logger.New(*new.Log))
			slog.Info("runtime: log config applied", "level", new.Log.Level, "format", new.Log.Format)
		}
	}
}

// rtmpListenerEnabled reports whether the shared RTMP listener should run.
func rtmpListenerEnabled(l *config.ListenersConfig) bool {
	return l != nil && l.RTMP.Enabled && l.RTMP.Port > 0
}

// rtspListenerEnabled reports whether the publisher RTSP listener should run.
func rtspListenerEnabled(l *config.ListenersConfig) bool {
	return l != nil && l.RTSP.Enabled && l.RTSP.Port > 0
}

// srtListenerEnabled reports whether the shared SRT listener should run.
func srtListenerEnabled(l *config.ListenersConfig) bool {
	return l != nil && l.SRT.Enabled && l.SRT.Port > 0
}

// rtmpListenerOf returns the RTMP listener config (or nil) for diff comparison.
func rtmpListenerOf(l *config.ListenersConfig) *config.RTMPListenerConfig {
	if l == nil {
		return nil
	}
	r := l.RTMP
	return &r
}

// rtspListenerOf returns the RTSP listener config (or nil) for diff comparison.
func rtspListenerOf(l *config.ListenersConfig) *config.RTSPListenerConfig {
	if l == nil {
		return nil
	}
	r := l.RTSP
	return &r
}

// srtListenerOf returns the SRT listener config (or nil) for diff comparison.
func srtListenerOf(l *config.ListenersConfig) *config.SRTListenerConfig {
	if l == nil {
		return nil
	}
	r := l.SRT
	return &r
}

// diffService handles the transition for a single service:
//   - was off, now on → start
//   - was on, now off → stop
//   - was on, now on, config changed → restart (stop + start)
//   - unchanged → no-op
func (m *Manager) diffService(
	name string,
	wasOn, nowOn bool,
	changed bool,
	startFn func(context.Context) error,
) {
	switch {
	case !wasOn && nowOn:
		slog.Info("runtime: starting service", "service", name)
		m.startService(name, startFn)
	case wasOn && !nowOn:
		slog.Info("runtime: stopping service", "service", name)
		m.stopService(name)
	case wasOn && nowOn && changed:
		slog.Info("runtime: restarting service (config changed)", "service", name)
		m.stopService(name)
		m.startService(name, startFn)
	}
}

func (m *Manager) startService(name string, fn func(context.Context) error) {
	ctx, cancel := context.WithCancel(m.rootCtx)
	done := make(chan struct{})

	m.mu.Lock()
	// Stop any existing instance first.
	if old, ok := m.services[name]; ok {
		old.cancel()
		m.mu.Unlock()
		<-old.done
		m.mu.Lock()
	}
	m.services[name] = &serviceEntry{cancel: cancel, done: done}
	m.mu.Unlock()

	go func() {
		defer close(done)
		if err := fn(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("runtime: service exited with error", "service", name, "err", err)
		}
	}()
}

func (m *Manager) stopService(name string) {
	m.mu.Lock()
	e, ok := m.services[name]
	if ok {
		delete(m.services, name)
	}
	m.mu.Unlock()
	if ok {
		e.cancel()
		<-e.done
		slog.Info("runtime: service stopped", "service", name)
	}
}

// configChanged compares two config sections by value.
// Returns true when the values differ or one is nil and the other is not.
func configChanged(a, b any) bool {
	if a == nil && b == nil {
		return false
	}
	if a == nil || b == nil {
		return true
	}
	return !reflect.DeepEqual(a, b)
}
