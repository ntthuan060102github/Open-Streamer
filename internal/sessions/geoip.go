package sessions

import "net"

// GeoIPResolver maps a remote IP to an ISO 3166-1 alpha-2 country code.
// Implementations must be safe for concurrent use and return "" when the
// address cannot be resolved (private address, missing DB, lookup error).
//
// The default registered in DI is NullGeoIP — no lookup, Country always "".
// When `sessions.geoip_db_path` is set in config, the wiring layer in
// `cmd/server/main.go` opens the .mmdb via NewMaxMindGeoIP and registers
// that as the resolver instead. Operators can also swap in any custom
// implementation (IP2Location, in-house service) by replacing the DI
// binding before service start.
type GeoIPResolver interface {
	Country(ip net.IP) string
}

// NullGeoIP is a GeoIPResolver that always returns "". Used when GeoIP is
// disabled or no real resolver has been wired. Public so external code can
// reference it as an explicit "disabled" marker.
type NullGeoIP struct{}

// Country implements GeoIPResolver — always returns the empty string.
func (NullGeoIP) Country(net.IP) string { return "" }
