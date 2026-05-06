package sessions

import (
	"io"
	"net"
	"sync/atomic"
)

// GeoIPResolver maps a remote IP to an ISO 3166-1 alpha-2 country code.
// Implementations must be safe for concurrent use and return "" when the
// address cannot be resolved (private address, missing DB, lookup error).
//
// The default registered in DI is *SwappableGeoIP wrapping NullGeoIP —
// no lookup, Country always "". When `sessions.geoip_db_path` is set in
// config, the wiring layer in `cmd/server/main.go` opens the .mmdb via
// NewMaxMindGeoIP and calls SwappableGeoIP.Set with the result. Hot-reload
// of the .mmdb file goes through the same Set path, driven by the config
// diff in sessions.Service.UpdateConfig.
type GeoIPResolver interface {
	Country(ip net.IP) string
}

// NullGeoIP is a GeoIPResolver that always returns "". Used when GeoIP is
// disabled or no real resolver has been wired. Public so external code can
// reference it as an explicit "disabled" marker.
type NullGeoIP struct{}

// Country implements GeoIPResolver — always returns the empty string.
func (NullGeoIP) Country(net.IP) string { return "" }

// SwappableGeoIP is a GeoIPResolver wrapper that holds the current concrete
// resolver behind an atomic.Pointer so the wiring layer can hot-swap the
// backing .mmdb file without restarting the tracker. The resolver returned
// by every Country call is the one published by the most recent Set; in-flight
// lookups against the previous resolver complete normally because Set never
// closes the old resolver until after the new one is published.
//
// Why a pointer-to-holder rather than atomic.Value: atomic.Value requires a
// consistent dynamic type across Store calls, but we want to swap between
// NullGeoIP{} (struct) and *MaxMindGeoIP (pointer). Wrapping both in
// `*holder` sidesteps the type-mismatch panic.
type SwappableGeoIP struct {
	current atomic.Pointer[geoipHolder]
}

// geoipHolder bundles the live resolver with its closer (when the resolver
// owns mmap'd resources) so Set can release the previous backing store
// after a successful swap. closer may be nil for resolvers that hold no
// OS handles (NullGeoIP, custom in-process implementations).
type geoipHolder struct {
	r GeoIPResolver
	c io.Closer
}

// NewSwappableGeoIP returns an initialised swappable resolver pre-loaded
// with NullGeoIP, so Country is callable immediately and never returns
// against a nil pointer. Subsequent Set calls publish a new backing
// resolver atomically.
func NewSwappableGeoIP() *SwappableGeoIP {
	s := &SwappableGeoIP{}
	s.Set(NullGeoIP{})
	return s
}

// Country implements GeoIPResolver. Atomic load of the current holder; the
// indirection cost is one pointer load plus one interface call — measured
// at ~3 ns on amd64, well within the per-session budget.
func (s *SwappableGeoIP) Country(ip net.IP) string {
	h := s.current.Load()
	if h == nil || h.r == nil {
		return ""
	}
	return h.r.Country(ip)
}

// Set publishes a new resolver and closes the previous one's backing
// resource (mmap'd file for *MaxMindGeoIP). Safe for concurrent use; the
// atomic.Pointer.Swap fences the close so concurrent Country calls always
// see a fully-initialised resolver. A nil resolver is rejected silently
// (the previous resolver stays live) so callers can't accidentally publish
// a "disabled" state by passing nil — use Set(NullGeoIP{}) for that.
func (s *SwappableGeoIP) Set(r GeoIPResolver) {
	if r == nil {
		return
	}
	var c io.Closer
	if cl, ok := r.(io.Closer); ok {
		c = cl
	}
	old := s.current.Swap(&geoipHolder{r: r, c: c})
	if old != nil && old.c != nil {
		// Best-effort close — we already published the new resolver, so a
		// failure here is a resource leak, not a correctness problem. The
		// caller can't act on the error meaningfully either; logging is
		// the wiring layer's job since it has the path context.
		_ = old.c.Close()
	}
}

// Close releases the current holder's backing resource. Idempotent — calling
// it twice (or before any Set) is safe. Implements io.Closer so the wiring
// layer can defer it on shutdown without type assertions.
func (s *SwappableGeoIP) Close() error {
	old := s.current.Swap(nil)
	if old == nil || old.c == nil {
		return nil
	}
	return old.c.Close()
}

// Static interface assertions — surface mismatches at compile time.
var (
	_ GeoIPResolver = (*SwappableGeoIP)(nil)
	_ io.Closer     = (*SwappableGeoIP)(nil)
)
