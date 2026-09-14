package externalref

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Guard enforces the fetch URL policy for every upstream metadata fetch
// (docs/23 §7 "SSRF guard for External Reference fetch、URL allow/deny
// strategy", docs/54 threat scenario #5 "External Reference fetch SSRF
// 内网"): https-only, and no connection into private/loopback/link-local/
// reserved address space — on every hop, including redirects. The policy
// is structural, not configurable at runtime: there is no "allow private"
// switch to weaken, and a future allowlist (docs/23 §7) will layer ON TOP
// of the structural blocks, never remove them.
type Guard struct {
	// lookup resolves a hostname to its addresses. It is a seam for
	// tests only: production uses the system resolver
	// (net.DefaultResolver.LookupIPAddr). Resolution is re-run per
	// check so a DNS rebind cannot slip a private answer past a cached
	// public one.
	lookup func(ctx context.Context, host string) ([]net.IPAddr, error)
}

// NewGuard builds the production guard on the system resolver.
func NewGuard() *Guard {
	return &Guard{lookup: net.DefaultResolver.LookupIPAddr}
}

// DefaultFetchTimeout bounds one whole fetch (all redirect hops): a
// manual refresh waits on the human's timescale, an upstream that cannot
// answer in 10s is unavailable for this refresh.
const DefaultFetchTimeout = 10 * time.Second

// NewFetchClient builds the guarded HTTP client every production fetch
// runs through: a wrapping RoundTripper validates EVERY request against
// the policy before it is sent (scheme + address space, first hop
// included), the transport's dial refuses private address space and then
// dials the CLASSIFIED IPs themselves — never the hostname (belt), and
// the redirect check re-validates every hop before it is followed
// (suspenders — a redirect target is re-resolved at follow time).
func (g *Guard) NewFetchClient(timeout time.Duration) *http.Client {
	guard := g
	if guard == nil {
		guard = NewGuard()
	}
	base := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, errf(ErrRefusedURL, "unparseable dial address %q", addr)
			}
			ips, err := guard.resolve(ctx, host)
			if err != nil {
				return nil, errf(ErrRefusedURL, "resolving %q: %v", host, err)
			}
			if len(ips) == 0 {
				return nil, errf(ErrRefusedURL, "host %q resolves to no addresses", host)
			}
			for _, ip := range ips {
				if err := classifyIP(ip); err != nil {
					return nil, errf(ErrRefusedURL, "refusing to dial %q: %v", addr, err)
				}
			}
			// Dial the classified batch, in order, as IP literals. The
			// hostname must NOT reach the dialer: a dial-by-hostname
			// would make the net.Dialer re-resolve through the system
			// resolver — a second resolution the guard never classified
			// (the DNS-rebind TOCTOU window). TLS keeps its ServerName
			// from the request URL, so dialing an IP cannot break
			// certificate verification.
			return dialChecked(ctx, network, ips, port, timeout)
		},
	}
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// The default policy caps redirects at 10; keep that and
			// add the structural check per hop.
			if len(via) >= 10 {
				return errors.New("externalref: stopped after 10 redirects")
			}
			if err := guard.CheckURL(req.Context(), req.URL); err != nil {
				return err
			}
			return nil
		},
		Transport: guardTransport{guard: guard, next: base},
	}
}

// dialChecked dials every address in the classified batch, in order, and
// returns the first connection that succeeds. The batch is expected to be
// fully classified by the caller (NewFetchClient's DialContext); dialing
// is by IP literal so no second resolution can ever substitute a
// different address for a checked one. Each attempt gets the full dial
// timeout, like the standard library's own multi-address fallback.
func dialChecked(ctx context.Context, network string, ips []net.IP, port string, timeout time.Duration) (net.Conn, error) {
	if len(ips) == 0 {
		return nil, errf(ErrRefusedURL, "no classified addresses to dial")
	}
	dialer := &net.Dialer{Timeout: timeout}
	var lastErr error
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, errf(ErrRefusedURL, "dialing the classified addresses failed: %v", lastErr)
}

// guardTransport runs the URL policy on every request before the base
// transport sees it — the FIRST hop included (CheckRedirect only sees
// follow-ups, and the dial guard only sees addresses). A caller that
// swaps Transport for its own takes over the URL policy responsibility.
type guardTransport struct {
	guard *Guard
	next  http.RoundTripper
}

func (gt guardTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := gt.guard.CheckURL(req.Context(), req.URL); err != nil {
		return nil, err
	}
	return gt.next.RoundTrip(req)
}

// CheckURL validates one URL against the fetch policy before any
// connection is made: the scheme must be https and every address the
// host resolves to must be public. It is the whole guard for a caller
// that brings its own transport; the guarded client above runs the same
// checks automatically on every hop.
func (g *Guard) CheckURL(ctx context.Context, u *url.URL) error {
	if u == nil {
		return errf(ErrRefusedURL, "fetch URL is missing")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return errf(ErrRefusedURL, "fetch URL scheme %q is not https", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return errf(ErrRefusedURL, "fetch URL has no host")
	}
	ips, err := g.resolve(ctx, host)
	if err != nil {
		return errf(ErrRefusedURL, "resolving %q: %v", host, err)
	}
	if len(ips) == 0 {
		return errf(ErrRefusedURL, "host %q resolves to no addresses", host)
	}
	for _, ip := range ips {
		if err := classifyIP(ip); err != nil {
			return errf(ErrRefusedURL, "refusing %q: %v", host, err)
		}
	}
	return nil
}

// resolve returns every address of host; an IP literal answers itself,
// a hostname is resolved through the guard's resolver.
func (g *Guard) resolve(ctx context.Context, host string) ([]net.IP, error) {
	guard := g
	if guard == nil {
		guard = NewGuard()
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return []net.IP{ip}, nil
	}
	addrs, err := guard.lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	return ips, nil
}

// classifyIP reports whether one resolved address is allowed as a fetch
// target: only globally routable, non-reserved unicast space passes. The
// blocked set is the SSRF-relevant whole of the IANA special-purpose
// registry — private, loopback, link-local (including the cloud metadata
// endpoint 169.254.169.254), CGNAT, documentation/reserved ranges,
// multicast, unspecified — plus IPv4-mapped IPv6 re-classified through
// its embedded IPv4 address. Everything else (a public address) is nil.
func classifyIP(ip net.IP) error {
	if ip == nil {
		return errors.New("not an IP address")
	}
	// IPv4-mapped IPv6 (::ffff:a.b.c.d) carries an IPv4 address that
	// must obey the IPv4 rules; Go's net.IP.To4 handles the mapping.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, block := range blockedV4 {
		if block.Contains(ip) {
			return fmt.Errorf("address %s is in blocked range %s", ip, block)
		}
	}
	if ip.To4() == nil {
		for _, block := range blockedV6 {
			if block.Contains(ip) {
				return fmt.Errorf("address %s is in blocked range %s", ip, block)
			}
		}
	}
	return nil
}

// blockedV4 is the IPv4 blocked set, ordered by the IANA registry:
// unspecified/this network, private, loopback, link-local, CGNAT,
// documentation/reserved, multicast and the reserved tail.
var blockedV4 = []*net.IPNet{
	ipnet("0.0.0.0/8"),       // "this network"
	ipnet("10.0.0.0/8"),      // private
	ipnet("100.64.0.0/10"),   // CGNAT
	ipnet("127.0.0.0/8"),     // loopback
	ipnet("169.254.0.0/16"),  // link-local (cloud metadata endpoint)
	ipnet("172.16.0.0/12"),   // private
	ipnet("192.0.0.0/24"),    // IETF protocol assignments
	ipnet("192.0.2.0/24"),    // documentation (TEST-NET-1)
	ipnet("192.168.0.0/16"),  // private
	ipnet("198.18.0.0/15"),   // benchmarking
	ipnet("198.51.100.0/24"), // documentation (TEST-NET-2)
	ipnet("203.0.113.0/24"),  // documentation (TEST-NET-3)
	ipnet("224.0.0.0/4"),     // multicast
	ipnet("240.0.0.0/4"),     // reserved
}

// blockedV6 is the IPv6 blocked set: unspecified, loopback,
// IPv4-mapped, unique-local, link-local, multicast, documentation.
var blockedV6 = []*net.IPNet{
	ipnet("::/128"),
	ipnet("::1/128"),
	ipnet("::ffff:0:0/96"),
	ipnet("64:ff9b::/96"),  // NAT64 well-known prefix
	ipnet("100::/64"),      // discard-only
	ipnet("2001:db8::/32"), // documentation
	ipnet("fc00::/7"),      // unique-local
	ipnet("fe80::/10"),     // link-local
	ipnet("ff00::/8"),      // multicast
}

func ipnet(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(fmt.Sprintf("externalref: bad blocked-range %q: %v", s, err))
	}
	return n
}
