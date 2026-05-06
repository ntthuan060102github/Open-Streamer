package sessions

// geoip_maxmind_test.go — wrapper edge cases for the MaxMind GeoIP
// resolver. Real lookup correctness (IP → country code) is the
// `oschwald/geoip2-golang` library's responsibility and is exercised by
// its own test suite — duplicating that here would just re-test their
// code with a fixture .mmdb we can't legally vendor (MaxMind's license
// restricts redistribution).
//
// What we DO guard:
//   - "no path configured" surfaces as an error so the DI factory can
//     fall through to NullGeoIP without booting with a half-working
//     resolver
//   - "path doesn't exist" same — never panic, never hang
//   - Country() never panics on nil receiver / nil IP / closed db; those
//     all degrade silently to "" so caller (PlaySession.Country) treats
//     them as "GeoIP unavailable" instead of crashing the session loop
//   - Close is idempotent so wiring layer can defer it without state
//     tracking

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewMaxMindGeoIPRejectsEmptyPath(t *testing.T) {
	t.Parallel()
	_, err := NewMaxMindGeoIP("")
	require.Error(t, err, "empty path must be rejected so the DI factory falls through to NullGeoIP")
	require.Contains(t, err.Error(), "empty")
}

func TestNewMaxMindGeoIPRejectsMissingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := NewMaxMindGeoIP(filepath.Join(dir, "does-not-exist.mmdb"))
	require.Error(t, err, "missing file must surface as an error rather than panic")
}

func TestNewMaxMindGeoIPRejectsNonMMDB(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bogus := filepath.Join(dir, "not-a-real.mmdb")
	require.NoError(t, os.WriteFile(bogus, []byte("this is not a maxmind database"), 0o644))

	_, err := NewMaxMindGeoIP(bogus)
	require.Error(t, err, "malformed .mmdb must be rejected at open, not at first lookup")
}

func TestMaxMindGeoIPCountryNilReceiverReturnsEmpty(t *testing.T) {
	t.Parallel()
	var m *MaxMindGeoIP
	require.Equal(t, "", m.Country(net.ParseIP("8.8.8.8")),
		"nil receiver must be safe — caller treats \"\" as GeoIP unavailable")
}

func TestMaxMindGeoIPCountryNilIPReturnsEmpty(t *testing.T) {
	t.Parallel()
	// Construct without going through Open by zero-valuing the struct;
	// the nil-IP guard fires before db is touched.
	m := &MaxMindGeoIP{}
	require.Equal(t, "", m.Country(nil))
}

func TestMaxMindGeoIPCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	var m *MaxMindGeoIP
	require.NoError(t, m.Close(), "nil receiver Close must be a no-op")

	m2 := &MaxMindGeoIP{} // db is nil — second call must also be safe
	require.NoError(t, m2.Close())
	require.NoError(t, m2.Close(), "double Close must not panic or error")
}
