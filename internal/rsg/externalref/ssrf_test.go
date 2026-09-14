package externalref

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// The SSRF policy is docs/23 §7 + docs/54 threat scenario #5 ("External
// Reference fetch SSRF 内网"): every fetch URL must be https and every
// address it resolves to must be publicly routable. These tests are the
// automated negative proof the threat model demands — each blocked range
// must actually be refused, not merely listed.

// TestClassifyIPBlocksEveryReservedRange walks the entire blocked set:
// the FIRST address of every declared block must be refused, and a
// representative PUBLIC address of each family must pass.
func TestClassifyIPBlocksEveryReservedRange(t *testing.T) {
	for _, block := range append(append([]*net.IPNet{}, blockedV4...), blockedV6...) {
		ip := block.IP // network address, inside the block
		if err := classifyIP(ip); err == nil {
			t.Errorf("classifyIP(%s in %s) = nil, want refusal", ip, block)
		}
	}
	// The broadcast edge of the last v4 block is inside it too.
	if err := classifyIP(net.ParseIP("255.255.255.255")); err == nil {
		t.Error("classifyIP(255.255.255.255) = nil, want refusal")
	}
	// Representative public addresses must pass.
	for _, pub := range []string{
		"8.8.8.8", "1.1.1.1", "104.16.0.1", // public v4
		"2606:4700:4700::1111", "2001:4860:4860::8888", // public v6
	} {
		if err := classifyIP(net.ParseIP(pub)); err != nil {
			t.Errorf("classifyIP(%s) = %v, want nil", pub, err)
		}
	}
	// IPv4-mapped IPv6 must be re-classified through its embedded IPv4:
	// ::ffff:10.0.0.1 is the private 10.0.0.1 in disguise and must be
	// refused even though 10.0.0.0/8 only names v4 space.
	if err := classifyIP(net.ParseIP("::ffff:10.0.0.1")); err == nil {
		t.Error("classifyIP(::ffff:10.0.0.1) = nil, want refusal (IPv4-mapped private)")
	}
	if err := classifyIP(net.ParseIP("::ffff:8.8.8.8")); err != nil {
		t.Errorf("classifyIP(::ffff:8.8.8.8) = %v, want nil (IPv4-mapped public)", err)
	}
	if err := classifyIP(nil); err == nil {
		t.Error("classifyIP(nil) = nil, want refusal")
	}
}

// TestGuardCheckURLRefusesPrivateResolution proves the resolver seam:
// whatever the resolver answers, every address is classified before the
// URL is accepted.
func TestGuardCheckURLRefusesPrivateResolution(t *testing.T) {
	ctx := context.Background()
	g := &Guard{lookup: func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("10.0.0.5")}}, nil
	}}
	u, _ := url.Parse("https://example.com/metadata")
	err := g.CheckURL(ctx, u)
	if !errors.Is(err, ErrRefusedURL) {
		t.Fatalf("CheckURL(private resolution) = %v, want ErrRefusedURL", err)
	}

	// A public answer passes.
	g.lookup = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	if err := g.CheckURL(ctx, u); err != nil {
		t.Fatalf("CheckURL(public resolution) = %v, want nil", err)
	}

	// One private address among public ones refuses the WHOLE host — a
	// multi-homed host with any blocked address must not be dialed.
	g.lookup = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{
			{IP: net.ParseIP("8.8.8.8")},
			{IP: net.ParseIP("192.168.1.1")},
		}, nil
	}
	if err := g.CheckURL(ctx, u); !errors.Is(err, ErrRefusedURL) {
		t.Fatalf("CheckURL(mixed public+private) = %v, want ErrRefusedURL", err)
	}
}

// TestGuardCheckURLSchemeAndShape proves the structural half of the
// policy: https only, a host is required, an unresolvable host is a
// refusal, and an IP literal is classified without a resolver.
func TestGuardCheckURLSchemeAndShape(t *testing.T) {
	ctx := context.Background()
	g := &Guard{lookup: func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return nil, fmt.Errorf("no such host")
	}}
	for _, raw := range []string{
		"http://example.com/x",                     // http scheme
		"ftp://example.com/x",                      // non-http scheme
		"https:///x",                               // no host
		"https://10.0.0.5/x",                       // IP-literal private target
		"https://127.0.0.1/x",                      // IP-literal loopback
		"https://[::1]/x",                          // IP-literal v6 loopback
		"https://[fe80::1]/x",                      // IP-literal v6 link-local
		"https://169.254.169.254/latest/meta-data", // cloud metadata endpoint
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		if err := g.CheckURL(ctx, u); !errors.Is(err, ErrRefusedURL) {
			t.Errorf("CheckURL(%s) = %v, want ErrRefusedURL", raw, err)
		}
	}
	if err := g.CheckURL(ctx, nil); !errors.Is(err, ErrRefusedURL) {
		t.Errorf("CheckURL(nil) = %v, want ErrRefusedURL", err)
	}
}

// TestGuardFetchClientRedirectToPrivateIsRefused is the end-to-end
// negative: a fetch that follows a redirect into private space is
// refused at the redirect hop, before any connection to the private
// address is made (docs/54 #5 — the redirect is the classic SSRF path,
// which is why the guarded client re-checks EVERY hop).
func TestGuardFetchClientRedirectToPrivateIsRefused(t *testing.T) {
	// The private target: a server that must never be reached — if the
	// guarded client dialed it, the test fails inside its handler.
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the guarded client dialed the private redirect target — the redirect must have been refused before any connection")
	}))
	defer private.Close()

	// Resolver: the start host is public, everything else resolves to
	// private space — the redirect target's hostname included.
	startHost := "public-start.example"
	g := &Guard{lookup: func(ctx context.Context, host string) ([]net.IPAddr, error) {
		if host == startHost {
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("10.0.0.9")}}, nil
	}}

	// The public entry point: a server that redirects to the private
	// hostname. It speaks TLS (the start URL is https), and it is reached
	// by swapping the base transport's dialer to the redirector's
	// listener — the guard still owns CheckRedirect and the RoundTripper
	// policy check, and those are the things on trial. Certificate
	// verification is off because the dial is synthetic (test-only).
	redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://internal.example/private", http.StatusFound)
	}))
	defer redirector.Close()

	client := g.NewFetchClient(DefaultFetchTimeout)
	base := client.Transport.(guardTransport).next.(*http.Transport)
	base.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	base.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return net.Dial("tcp", redirector.Listener.Addr().String())
	}

	req, _ := http.NewRequest(http.MethodGet, "https://"+startHost+"/start", nil)
	_, err := client.Do(req)
	if err == nil {
		t.Fatal("fetch through a private redirect succeeded — the redirect hop must be refused")
	}
	// The refusal is the SSRF policy on the redirect target, not a dial
	// failure.
	if !errors.Is(err, ErrRefusedURL) {
		t.Fatalf("redirect fetch error = %v, want ErrRefusedURL", err)
	}
}

// TestGuardFetchClientHTTPSchemeRefused proves the guarded client itself
// refuses an http URL on the FIRST hop — before any dial (the wrapping
// RoundTripper runs the policy per request; CheckRedirect only ever sees
// follow-ups).
func TestGuardFetchClientHTTPSchemeRefused(t *testing.T) {
	g := &Guard{lookup: func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}}
	client := g.NewFetchClient(DefaultFetchTimeout)
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/x", nil)
	_, err := client.Do(req)
	if !errors.Is(err, ErrRefusedURL) {
		t.Fatalf("http fetch error = %v, want ErrRefusedURL", err)
	}
}

// rebindListener is a loopback listener that counts every accepted
// connection. It is the "local server" a rebind must never reach; a test
// that connects to it fails inside its counter.
func rebindListener(t *testing.T) (addr string, hits *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	var count atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			count.Add(1)
			conn.Close()
		}
	}()
	return ln.Addr().String(), &count
}

// noProxyTransport pins the base transport of a guarded client to
// direct dialing: an ambient http_proxy would otherwise route these
// test URLs to a proxy (whose address is environment-dependent) instead
// of exercising the guarded dial. The guard's own checks stay untouched.
func noProxyTransport(t *testing.T, client *http.Client) *http.Transport {
	t.Helper()
	base, ok := client.Transport.(guardTransport).next.(*http.Transport)
	if !ok {
		t.Fatalf("guarded client transport is %T, want guardTransport over *http.Transport", client.Transport)
	}
	base.Proxy = nil
	return base
}

// TestGuardFetchClientDialsCheckedAddresses is the DNS-rebind TOCTOU
// proof for the dial itself: the URL check and the dial are two
// resolutions, and an attacker who flips the answer between them (the
// check sees public space, the dial sees loopback) must still never
// reach a loopback listener. The old implementation dialed by HOSTNAME
// after classifying the checked answers — the net.Dialer then
// re-resolved through the SYSTEM resolver, a second resolution the guard
// never saw: a URL whose hostname spells "localhost" with a public
// checked answer still connected to the local server. The fixed dial
// connects only to the classified IPs themselves, so the transport can
// never reach an address the guard has not classified.
func TestGuardFetchClientDialsCheckedAddresses(t *testing.T) {
	addr, hits := rebindListener(t)
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split listener address %q: %v", addr, err)
	}

	// The lying resolver: every question about the host gets a PUBLIC
	// answer — but the host is spelled "localhost", so a dial-by-hostname
	// reaches the loopback listener through the SYSTEM resolver, past the
	// guard. That is the rebind window: two resolutions, one check.
	g := &Guard{lookup: func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}}
	client := g.NewFetchClient(3 * time.Second)
	noProxyTransport(t, client)

	req, err := http.NewRequest(http.MethodGet, "https://localhost:"+port+"/rebind", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if _, err := client.Do(req); err == nil {
		t.Fatal("rebind fetch succeeded — the checked addresses are public; the request must not reach the local listener")
	}
	if hits.Load() != 0 {
		t.Fatalf("the request reached the loopback listener: the dial connected to an address the guard never classified (DNS rebind)")
	}
}

// TestGuardFetchClientRefusesResolverFlipToLoopback is the flip variant
// of the rebind: the first resolution (the URL check) answers public,
// the second (the dial) answers loopback. The dial re-classifies its own
// batch at dial time and refuses before any connection is made — a
// regression pin for the checked-IP dialing.
func TestGuardFetchClientRefusesResolverFlipToLoopback(t *testing.T) {
	addr, hits := rebindListener(t)
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split listener address %q: %v", addr, err)
	}

	var calls atomic.Int32
	g := &Guard{lookup: func(ctx context.Context, host string) ([]net.IPAddr, error) {
		if calls.Add(1) == 1 {
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}}
	client := g.NewFetchClient(3 * time.Second)
	noProxyTransport(t, client)

	req, err := http.NewRequest(http.MethodGet, "https://localhost:"+port+"/flip", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if _, err := client.Do(req); !errors.Is(err, ErrRefusedURL) {
		t.Fatalf("flip fetch error = %v, want ErrRefusedURL (the dial must re-classify its own answers)", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("the request reached the loopback listener — the flipped answer must be refused before any dial")
	}
}

// TestDialCheckedTriesEachAddressInOrder pins the multi-address dial:
// dialChecked walks the classified batch in order, dialing each address
// as an IP literal, and returns the first connection that succeeds.
func TestDialCheckedTriesEachAddressInOrder(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	accepted := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			conn.Close()
		}
		close(accepted)
	}()

	// 127.0.0.2:port has no listener (the whole 127/8 is loopback, so the
	// attempt fails with an immediate refusal); the second address must
	// still be tried and win.
	conn, err := dialChecked(context.Background(), "tcp",
		[]net.IP{net.ParseIP("127.0.0.2"), net.ParseIP("127.0.0.1")}, port, 3*time.Second)
	if err != nil {
		t.Fatalf("dialChecked did not fall through to the second address: %v", err)
	}
	defer conn.Close()
	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("the returned connection did not reach the listener")
	}

	// An empty batch dials nothing and fails rather than fabricating a
	// connection.
	if conn, err := dialChecked(context.Background(), "tcp", nil, port, time.Second); err == nil {
		conn.Close()
		t.Fatal("dialChecked with no addresses returned a connection")
	}
}

// TestTransportKeepsServerNameFromURL pins the TLS property the
// checked-IP dialing depends on: when DialContext dials an IP literal,
// the handshake still presents the URL's HOSTNAME as ServerName (Go's
// transport takes ServerName from the request URL, never from the dial
// address), so dialing a classified IP cannot break certificate
// verification. The guarded client cannot reach a local listener by
// construction (the guard refuses loopback), so the proof runs on a
// transport shaped exactly like the guarded one: DialContext replaces
// the hostname with the listener's address, the URL keeps the hostname.
func TestTransportKeepsServerNameFromURL(t *testing.T) {
	const host = "external.example"
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}

	var sni atomic.Value
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{cert},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			sni.Store(hello.ServerName)
			return &cert, nil
		},
	}
	srv.StartTLS()
	defer srv.Close()

	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)

	client := &http.Client{Transport: &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// Dial the listener's address the way the guarded transport
			// dials classified IPs: the URL hostname never reaches the
			// dialer.
			return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
		},
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}}
	resp, err := client.Get("https://" + host + "/x")
	if err != nil {
		t.Fatalf("GET through an IP-dialing transport: %v — with ServerName from the URL the handshake must verify against the hostname certificate", err)
	}
	defer resp.Body.Close()
	if got, _ := sni.Load().(string); got != host {
		t.Fatalf("ServerName presented to the server = %q, want %q — the transport must take ServerName from the request URL, not from the dialed address", got, host)
	}
}
