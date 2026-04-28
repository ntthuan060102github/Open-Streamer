package sessions

import "net"

// GeoIPResolver maps a remote IP to an ISO 3166-1 alpha-2 country code.
// Implementations must be safe for concurrent use and return "" when the
// address cannot be resolved (private address, missing DB, lookup error).
//
// The default implementation registered by the wiring layer is a no-op
// (always ""); operators wanting real GeoIP can swap in a MaxMind / IP2Location
// backed implementation by replacing the DI binding before service start —
// the sessions package intentionally has no MaxMind dependency to keep the
// binary lean and license-free.
type GeoIPResolver interface {
	Country(ip net.IP) string
}

// NullGeoIP is a GeoIPResolver that always returns "". Used when GeoIP is
// disabled or no real resolver has been wired. Public so external code can
// reference it as an explicit "disabled" marker.
type NullGeoIP struct{}

// Country implements GeoIPResolver — always returns the empty string.
func (NullGeoIP) Country(net.IP) string { return "" }
