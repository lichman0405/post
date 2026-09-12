// Package oidctest builds an in-process fake OpenID Connect provider for
// tests: discovery, JWKS, authorization and token endpoints, issuing real
// RS256-signed id_tokens over a real RSA key pair. It implements
// authn.OIDCProvider on the client side too, but its primary use is as a
// black-box HTTP IdP: tests drive the REAL authn.OIDCClient against it,
// so discovery, JWKS fetch, signature verification and claim checks run
// exactly as they will against a production provider.
//
// This is test infrastructure shipped as a normal package so the unit
// suite and the e2e suite share one implementation (a test-only package
// could not be imported by tests/e2e).
package oidctest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"time"

	"github.com/lichman0405/post/internal/application/authn"
)

// Provider is a fake OIDC IdP.
type Provider struct {
	mu    sync.Mutex
	key   *rsa.PrivateKey
	codes map[string]codeRecord
	ts    *httptest.Server
	// issuer is the provider's base URL (set once Serve starts).
	issuer string
	// rawIDToken, when set, is returned verbatim as the id_token of the
	// next successful exchange, bypassing the provider's own signing.
	// Signature-verification negative tests inject attacker-controlled
	// tokens through the real exchange path this way.
	rawIDToken string
}

type codeRecord struct {
	claims  authn.OIDCClaims
	expires time.Time
}

// Person describes one identity the fake provider can authenticate.
type Person struct {
	Subject           string
	Email             string
	EmailVerified     bool
	PreferredUsername string
	Name              string
}

// NewProvider builds a provider with a fresh RSA key; Serve must be called
// before it answers anything.
func NewProvider() *Provider {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err) // test setup failure: a broken rand source
	}
	return &Provider{key: key, codes: map[string]codeRecord{}}
}

// Serve starts the HTTP IdP and returns its issuer URL. Close stops it.
func (p *Provider) Serve() string {
	p.ts = httptest.NewServer(http.HandlerFunc(p.handle))
	p.issuer = p.ts.URL
	return p.issuer
}

// Close stops the IdP.
func (p *Provider) Close() {
	if p.ts != nil {
		p.ts.Close()
	}
}

// Issuer returns the base URL (empty before Serve).
func (p *Provider) Issuer() string { return p.issuer }

// PublicKey exposes the signing key (tests asserting signature details).
func (p *Provider) PublicKey() *rsa.PublicKey { return &p.key.PublicKey }

// SeedRawIDToken forces the next successful /token response to carry raw as
// its id_token. One-shot: consumed by the first exchange.
func (p *Provider) SeedRawIDToken(raw string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rawIDToken = raw
}

// IssueCode creates an authorization code for person (the code the
// authorization endpoint would have produced after login).
func (p *Provider) IssueCode(person Person) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	code := base64.RawURLEncoding.EncodeToString(b)
	p.codes[code] = codeRecord{claims: authn.OIDCClaims{
		Subject:           person.Subject,
		Email:             person.Email,
		EmailVerified:     person.EmailVerified,
		PreferredUsername: person.PreferredUsername,
		Name:              person.Name,
	}, expires: time.Now().Add(5 * time.Minute)}
	return code
}

// handle serves the IdP endpoints.
func (p *Provider) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/.well-known/openid-configuration":
		p.serveDiscovery(w)
	case r.URL.Path == "/jwks":
		p.serveJWKS(w)
	case r.URL.Path == "/authorize":
		p.serveAuthorize(w, r)
	case r.URL.Path == "/token":
		p.serveToken(w, r)
	case r.URL.Path == "/userinfo":
		p.serveUserinfo(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (p *Provider) serveDiscovery(w http.ResponseWriter) {
	writeJSON(w, map[string]any{
		"issuer":                 p.issuer,
		"authorization_endpoint": p.issuer + "/authorize",
		"token_endpoint":         p.issuer + "/token",
		"userinfo_endpoint":      p.issuer + "/userinfo",
		"jwks_uri":               p.issuer + "/jwks",
	})
}

// serveJWKS exports the public key in JWK form (n, e base64url).
func (p *Provider) serveJWKS(w http.ResponseWriter) {
	keyID := "test-key-1"
	e := big.NewInt(int64(p.key.PublicKey.E)).Bytes()
	writeJSON(w, map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA",
			"kid": keyID,
			"use": "sig",
			"alg": "RS256",
			"n":   base64.RawURLEncoding.EncodeToString(p.key.PublicKey.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(e),
		}},
	})
}

// serveAuthorize issues a code for the hardcoded test identity and
// redirects back to redirect_uri with code+state. Like a real IdP, it
// refuses a missing redirect_uri (an empty one is a client bug).
func (p *Provider) serveAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect := q.Get("redirect_uri")
	if redirect == "" {
		http.Error(w, "missing redirect_uri", http.StatusBadRequest)
		return
	}
	// The fake IdP authenticates exactly one identity (the test seeds
	// which one via IssueCodePerson); the browser dance is collapsed: the
	// test normally drives /authorize with a pre-issued code through
	// RedirectWithCode instead. When the endpoint is hit directly it
	// falls back to the default person.
	person := Person{
		Subject:           "sub-default",
		Email:             "oidc@example.com",
		EmailVerified:     true,
		PreferredUsername: "oidcuser",
		Name:              "OIDC User",
	}
	code := p.IssueCode(person)
	u, _ := url.Parse(redirect)
	q2 := u.Query()
	q2.Set("code", code)
	q2.Set("state", q.Get("state"))
	u.RawQuery = q2.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// serveToken exchanges an authorization code for an id_token + access
// token. The id_token is a REAL RS256 JWT signed with the provider key.
func (p *Provider) serveToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	rec, ok := p.codes[r.PostForm.Get("code")]
	rawID := ""
	if ok && time.Now().Before(rec.expires) {
		delete(p.codes, r.PostForm.Get("code")) // one-time use
		rawID = p.rawIDToken
		p.rawIDToken = "" // the seeded token is one-shot too
	}
	p.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		return
	}
	idToken := rawID
	if idToken == "" {
		var err error
		idToken, err = p.signIDToken(rec.claims)
		if err != nil {
			http.Error(w, "signing failure", http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, map[string]any{
		"access_token": "fake-access-token",
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     idToken,
	})
}

// signIDToken builds a valid RS256 JWT with the standard claims.
func (p *Provider) signIDToken(claims authn.OIDCClaims) (string, error) {
	now := time.Now()
	header := map[string]any{"alg": "RS256", "kid": "test-key-1", "typ": "JWT"}
	payload := map[string]any{
		"iss":                p.issuer,
		"sub":                claims.Subject,
		"aud":                "post-test-client", // the fake client id
		"exp":                now.Add(5 * time.Minute).Unix(),
		"iat":                now.Unix(),
		"email":              claims.Email,
		"email_verified":     claims.EmailVerified,
		"preferred_username": claims.PreferredUsername,
		"name":               claims.Name,
	}
	return signJWT(p.key, header, payload)
}

// SignIDTokenWith lets tests mint custom tokens (bad issuer, expired,
// unverified email) through the real signing path.
func (p *Provider) SignIDTokenWith(payload map[string]any) (string, error) {
	header := map[string]any{"alg": "RS256", "kid": "test-key-1", "typ": "JWT"}
	return signJWT(p.key, header, payload)
}

// signJWT signs header.payload with RS256 and returns the compact JWS.
func signJWT(key *rsa.PrivateKey, header, payload map[string]any) (string, error) {
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	pb, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(pb)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// serveUserinfo returns the default identity's userinfo claims.
func (p *Provider) serveUserinfo(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer fake-access-token" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, map[string]any{
		"sub":                "sub-default",
		"email":              "oidc@example.com",
		"email_verified":     true,
		"preferred_username": "oidcuser",
		"name":               "OIDC User",
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ClientID is the client id the fake provider issues tokens for.
const ClientID = "post-test-client"

// ClientSecret is the matching fake client secret.
const ClientSecret = "post-test-secret"

// NewClient builds the REAL authn.OIDCClient pointed at the fake provider
// (the exact production client, no mocks).
func (p *Provider) NewClient(redirectURI string) *authn.OIDCClient {
	return authn.NewOIDCClient(p.issuer, ClientID, ClientSecret, redirectURI)
}
