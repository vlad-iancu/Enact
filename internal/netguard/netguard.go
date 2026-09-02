// Package netguard refuses connections to addresses a service must never
// reach on a user's behalf.
//
// It is its own package because the rule is not about crawling. It applies
// wherever the platform fetches a URL somebody else chose, from the platform's
// own network position — which is now the web crawler, an issue tracker's base
// URL, and whatever comes next. Having it in one place is the difference
// between a rule and a habit: the JIRA source was written with a plain
// http.Client and could reach 127.0.0.1 and the cloud metadata endpoint,
// because the guard lived inside the crawler and nobody thought to look.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// ErrPrivateAddress means a host resolved to an address inside the
// deployment's own network.
var ErrPrivateAddress = errors.New("netguard: refusing to connect to a private address")

// Dialer returns a DialContext that refuses private, loopback, link-local and
// metadata addresses.
//
// The check is on the RESOLVED IP, inside the dialer, and that placement is
// the whole point. A check on the hostname is defeated by a DNS record
// pointing at 127.0.0.1; a check that resolves once and then dials is defeated
// by DNS rebinding, where the second lookup returns something different. Here
// the address being connected to is the address that was checked.
//
// allowPrivate disables it, which the test suite needs to reach an httptest
// server and which must never be set in a deployment that accepts URLs from
// users.
func Dialer(allowPrivate bool) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if allowPrivate {
			return dialer.DialContext(ctx, network, addr)
		}
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if IsPrivate(ip.IP) {
				return nil, fmt.Errorf("%w: %s resolves to %s", ErrPrivateAddress, host, ip.IP)
			}
		}
		return dialer.DialContext(ctx, network, addr)
	}
}

// IsPrivate reports whether an address is one the platform must not reach for
// a user.
func IsPrivate(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() ||
		// The cloud metadata endpoint is link-local and so already covered,
		// but it is named here because it is the reason this exists.
		ip.Equal(net.IPv4(169, 254, 169, 254))
}
