package sessions

// geoip_swappable_test.go — guards the SwappableGeoIP hot-reload contract.
//
// Three things must hold for the production hot-reload path to be safe:
//   1. Country() must always be callable, never see a nil-pointer state,
//      even before the first Set.
//   2. Set must atomically publish the new resolver and close the previous
//      one's backing resource (mmap'd .mmdb file).
//   3. Concurrent Country + Set must not race or panic.

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeGeoIP returns a fixed country code per IP and tracks Close calls so
// the swap-out behaviour can be asserted. Implements GeoIPResolver and
// io.Closer so it exercises the closer-aware path inside SwappableGeoIP.Set.
type fakeGeoIP struct {
	code       string
	closeCalls atomic.Int32
	closeErr   error
}

func (f *fakeGeoIP) Country(net.IP) string { return f.code }

func (f *fakeGeoIP) Close() error {
	f.closeCalls.Add(1)
	return f.closeErr
}

// New() pre-loads NullGeoIP so Country is callable immediately — the
// production wiring relies on this so tracker open paths never see "" from
// a nil resolver vs "" from a real lookup miss being indistinguishable.
func TestSwappableGeoIPDefaultIsNull(t *testing.T) {
	t.Parallel()
	s := NewSwappableGeoIP()
	require.Equal(t, "", s.Country(net.ParseIP("8.8.8.8")),
		"default backend must be NullGeoIP (empty country)")
}

// Set publishes a new resolver atomically — Country immediately returns
// values from the new backend after Set returns.
func TestSwappableGeoIPSetReplacesBackend(t *testing.T) {
	t.Parallel()
	s := NewSwappableGeoIP()
	s.Set(&fakeGeoIP{code: "VN"})
	require.Equal(t, "VN", s.Country(net.ParseIP("8.8.8.8")))

	s.Set(&fakeGeoIP{code: "US"})
	require.Equal(t, "US", s.Country(net.ParseIP("8.8.8.8")))
}

// Swapping in a new resolver must close the previous one when it
// implements io.Closer. Without this the .mmdb mmap leaks every reload.
func TestSwappableGeoIPSetClosesPreviousResolver(t *testing.T) {
	t.Parallel()
	s := NewSwappableGeoIP()
	first := &fakeGeoIP{code: "VN"}
	s.Set(first)
	require.Equal(t, int32(0), first.closeCalls.Load(),
		"resolver must not be closed while it is the live backend")

	second := &fakeGeoIP{code: "US"}
	s.Set(second)
	require.Equal(t, int32(1), first.closeCalls.Load(),
		"swapped-out resolver must be closed exactly once")
	require.Equal(t, int32(0), second.closeCalls.Load(),
		"newly-published resolver must not be closed by Set")
}

// Resolvers without io.Closer (e.g. NullGeoIP) must not crash the swap.
// Mixing closer + non-closer types in adjacent Set calls is the normal
// hot-reload pattern when an operator clears geoip_db_path.
func TestSwappableGeoIPSetHandlesNonCloserResolver(t *testing.T) {
	t.Parallel()
	s := NewSwappableGeoIP()
	closer := &fakeGeoIP{code: "VN"}
	s.Set(closer)

	require.NotPanics(t, func() { s.Set(NullGeoIP{}) },
		"swapping closer-resolver to non-closer must not panic")
	require.Equal(t, int32(1), closer.closeCalls.Load(),
		"the swapped-out closer resolver must still be closed")
	require.Equal(t, "", s.Country(net.ParseIP("8.8.8.8")),
		"NullGeoIP backend returns empty country")
}

// Set(nil) is rejected silently — accepting nil would publish a
// nil-resolver state that Country would have to special-case on every
// call. Better to treat the previous resolver as the floor.
func TestSwappableGeoIPSetNilIsNoop(t *testing.T) {
	t.Parallel()
	s := NewSwappableGeoIP()
	s.Set(&fakeGeoIP{code: "VN"})
	s.Set(nil)
	require.Equal(t, "VN", s.Country(net.ParseIP("8.8.8.8")),
		"Set(nil) must leave the previous resolver in place")
}

// Close releases the live backing resource and is idempotent — safe to
// defer in the wiring layer without an "is the swap initialised" check.
func TestSwappableGeoIPCloseReleasesAndIdempotent(t *testing.T) {
	t.Parallel()
	s := NewSwappableGeoIP()
	r := &fakeGeoIP{code: "VN"}
	s.Set(r)

	require.NoError(t, s.Close())
	require.Equal(t, int32(1), r.closeCalls.Load())

	require.NoError(t, s.Close(),
		"second Close must be a no-op, not a double-close")
}

// Close propagates the resolver's Close error — operators get visibility
// into mmap-release failures (rare but possible on filesystem teardown).
func TestSwappableGeoIPClosePropagatesError(t *testing.T) {
	t.Parallel()
	s := NewSwappableGeoIP()
	wantErr := errors.New("mmap release failed")
	s.Set(&fakeGeoIP{code: "VN", closeErr: wantErr})

	got := s.Close()
	require.ErrorIs(t, got, wantErr)
}

// Concurrent Country + Set must not race or panic. Run under -race to
// catch any non-atomic publication regression.
func TestSwappableGeoIPConcurrentSetAndCountry(t *testing.T) {
	t.Parallel()
	s := NewSwappableGeoIP()
	const writers = 4
	const readersPerWriter = 8
	const iters = 200

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				s.Set(&fakeGeoIP{code: "AA"})
			}
			_ = id
		}(w)
		for r := 0; r < readersPerWriter; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < iters; i++ {
					_ = s.Country(net.ParseIP("8.8.8.8"))
				}
			}()
		}
	}
	wg.Wait()
}
