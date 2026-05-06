package sessions

// geoip_maxmind.go — concrete GeoIPResolver backed by a MaxMind .mmdb
// database (GeoLite2-Country / GeoLite2-City; commercial GeoIP2 also works).
//
// The reader opens the file once at construction and serves all queries from
// the in-memory mmap'd database. It is safe for concurrent use — that is the
// `oschwald/maxminddb-golang` library's documented contract.
//
// File lifecycle: held open for the lifetime of the process. Re-loading
// after a database refresh requires constructing a new MaxMindGeoIP and
// swapping it in via DI — the existing reader keeps serving until then,
// so there is no lookup window with NullGeoIP fallback during a refresh.

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"

	"github.com/oschwald/geoip2-golang"
)

// MaxMindGeoIP wraps a MaxMind .mmdb reader and resolves IPs into ISO 3166-1
// alpha-2 country codes for PlaySession.Country.
type MaxMindGeoIP struct {
	db   *geoip2.Reader
	path string
}

// NewMaxMindGeoIP opens the .mmdb file at path and returns a resolver that
// looks up the Country code per IP. Returns an error when the file is
// missing, malformed, or not a Country / City database (those are the two
// schemas that surface a Country.IsoCode field; ASN / ISP databases do not).
func NewMaxMindGeoIP(path string) (*MaxMindGeoIP, error) {
	if path == "" {
		return nil, errors.New("geoip: db path is empty")
	}
	db, err := geoip2.Open(path)
	if err != nil {
		return nil, fmt.Errorf("geoip: open %q: %w", path, err)
	}
	return &MaxMindGeoIP{db: db, path: path}, nil
}

// Country implements GeoIPResolver. Returns "" when the IP is unroutable,
// not in the database, or any error path the underlying lookup hits — the
// caller (PlaySession.Country) treats "" as "GeoIP unavailable", so silent
// fallback is the right behaviour. Per-call errors are logged at debug to
// avoid log spam under heavy session churn (closed CDN sessions can hit
// "address not found" thousands of times per minute).
func (m *MaxMindGeoIP) Country(ip net.IP) string {
	if m == nil || m.db == nil || ip == nil {
		return ""
	}
	rec, err := m.db.Country(ip)
	if err != nil {
		slog.Debug("geoip: lookup failed", "ip", ip.String(), "err", err)
		return ""
	}
	return rec.Country.IsoCode
}

// Close releases the underlying mmap'd database file. Idempotent. Implements
// io.Closer so the wiring layer can call it on shutdown without type
// assertions; callers that hold a *MaxMindGeoIP can also defer it directly.
func (m *MaxMindGeoIP) Close() error {
	if m == nil || m.db == nil {
		return nil
	}
	err := m.db.Close()
	m.db = nil
	return err
}

// Static interface assertions — surface mismatches at compile time so a
// future Resolver / Closer signature drift fails the build instead of
// silently bypassing GeoIP at runtime.
var (
	_ GeoIPResolver = (*MaxMindGeoIP)(nil)
	_ io.Closer     = (*MaxMindGeoIP)(nil)
)
