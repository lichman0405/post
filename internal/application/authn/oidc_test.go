package authn_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/authn/oidctest"
)

// TestOIDCClientFullExchange drives the REAL client through the REAL
// provider: discovery, code exchange, RS256 verification over JWKS, claim
// checks — the production path, no mocks.
func TestOIDCClientFullExchange(t *testing.T) {
	provider := oidctest.NewProvider()
	issuer := provider.Serve()
	defer provider.Close()

	client := provider.NewClient("https://api.example.com/api/v1/auth/oidc/callback")
	url, err := client.AuthorizeURL(context.Background(), "https://api.example.com/api/v1/auth/oidc/callback", "state-123")
	if err != nil {
		t.Fatalf("AuthorizeURL: %v", err)
	}
	if !strings.HasPrefix(url, issuer+"/authorize?") {
		t.Errorf("authorize url = %q, want prefix %q", url, issuer+"/authorize?")
	}
	if !strings.Contains(url, "response_type=code") || !strings.Contains(url, "state=state-123") {
		t.Errorf("authorize url %q missing code/state params", url)
	}
	if !strings.Contains(url, "redirect_uri=https%3A%2F%2Fapi.example.com%2Fapi%2Fv1%2Fauth%2Foidc%2Fcallback") {
		t.Errorf("authorize url %q missing the request-derived redirect_uri", url)
	}

	code := provider.IssueCode(oidctest.Person{
		Subject:           "sub-alice",
		Email:             "Alice@Example.com",
		EmailVerified:     true,
		PreferredUsername: "alice",
		Name:              "Alice Researcher",
	})
	claims, err := client.ExchangeCode(context.Background(), code, "https://api.example.com/api/v1/auth/oidc/callback")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if claims.Email != "alice@example.com" {
		t.Errorf("claims email = %q, want normalized alice@example.com", claims.Email)
	}
	if !claims.EmailVerified || claims.Subject != "sub-alice" {
		t.Errorf("claims = %+v, want verified sub-alice", claims)
	}

	// The code is one-time use.
	if _, err := client.ExchangeCode(context.Background(), code, "https://api.example.com/api/v1/auth/oidc/callback"); err == nil {
		t.Error("replayed code accepted — codes must be single-use")
	}
}

// exchangeInjected runs a token exchange that returns an
// attacker-controlled id_token: the test seeds the raw token into the
// provider and exchanges a real code, so the client's full discovery +
// JWKS + verification path runs against hostile input.
func exchangeInjected(t *testing.T, provider *oidctest.Provider, client *authn.OIDCClient, rawIDToken string) (authn.OIDCClaims, error) {
	t.Helper()
	provider.SeedRawIDToken(rawIDToken)
	code := provider.IssueCode(oidctest.Person{Subject: "s", Email: "a@b.co", EmailVerified: true})
	return client.ExchangeCode(context.Background(), code, "https://api/cb")
}

// jwtEncode base64url-encodes a raw JWT segment without padding.
func jwtEncode(raw string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func TestOIDCClientRejectsTamperedSignature(t *testing.T) {
	provider := oidctest.NewProvider()
	provider.Serve()
	defer provider.Close()
	client := provider.NewClient("https://api/cb")

	// Mint a token with attacker claims, then swap the payload for a
	// victim identity while keeping the signature: re-signing is
	// impossible without the key, so verification must fail.
	valid, err := provider.SignIDTokenWith(map[string]any{
		"iss": provider.Issuer(), "sub": "attacker", "aud": oidctest.ClientID,
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"email": "attacker@example.com", "email_verified": true,
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	parts := strings.Split(valid, ".")
	evilPayload := jwtEncode(`{"iss":"` + provider.Issuer() + `","sub":"victim","aud":"` + oidctest.ClientID +
		`","exp":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) +
		`,"email":"victim@example.com","email_verified":true}`)
	forged := parts[0] + "." + evilPayload + "." + parts[2]

	if _, err := exchangeInjected(t, provider, client, forged); err == nil {
		t.Fatal("forged id_token accepted — signature verification broken")
	} else if !strings.Contains(err.Error(), "signature") {
		t.Errorf("error = %v, want signature failure", err)
	}
}

func TestOIDCClientRejectsWrongIssuerAudienceExpiry(t *testing.T) {
	provider := oidctest.NewProvider()
	provider.Serve()
	defer provider.Close()
	client := provider.NewClient("https://api/cb")
	now := time.Now()

	cases := []struct {
		name    string
		payload map[string]any
		wantErr string
	}{
		{"wrong issuer", map[string]any{
			"iss": "https://evil.example", "sub": "s", "aud": oidctest.ClientID,
			"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(),
			"email": "a@b.co", "email_verified": true}, "issuer"},
		{"wrong audience", map[string]any{
			"iss": provider.Issuer(), "sub": "s", "aud": "some-other-client",
			"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(),
			"email": "a@b.co", "email_verified": true}, "audience"},
		{"expired", map[string]any{
			"iss": provider.Issuer(), "sub": "s", "aud": oidctest.ClientID,
			"exp": now.Add(-time.Hour).Unix(), "iat": now.Add(-2 * time.Hour).Unix(),
			"email": "a@b.co", "email_verified": true}, "expired"},
		{"missing exp", map[string]any{
			"iss": provider.Issuer(), "sub": "s", "aud": oidctest.ClientID,
			"iat":   now.Unix(),
			"email": "a@b.co", "email_verified": true}, "exp"},
		{"future issued-at", map[string]any{
			"iss": provider.Issuer(), "sub": "s", "aud": oidctest.ClientID,
			"exp": now.Add(time.Hour).Unix(), "iat": now.Add(time.Hour).Unix(),
			"email": "a@b.co", "email_verified": true}, "future"},
		{"missing sub", map[string]any{
			"iss": provider.Issuer(), "aud": oidctest.ClientID,
			"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(),
			"email": "a@b.co", "email_verified": true}, "sub"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token, err := provider.SignIDTokenWith(tc.payload)
			if err != nil {
				t.Fatalf("sign: %v", err)
			}
			_, err = exchangeInjected(t, provider, client, token)
			if err == nil {
				t.Fatalf("accepted token with %s", tc.name)
			}
			if !strings.Contains(strings.ToLower(err.Error()), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestOIDCClientRejectsAlgNone(t *testing.T) {
	provider := oidctest.NewProvider()
	provider.Serve()
	defer provider.Close()
	client := provider.NewClient("https://api/cb")

	// alg=none token: unsigned payload, empty signature. The fixed-RS256
	// design must reject it before any signature math runs.
	payload := jwtEncode(`{"iss":"` + provider.Issuer() + `","sub":"s","aud":"` + oidctest.ClientID +
		`","exp":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) +
		`,"email":"a@b.co","email_verified":true}`)
	forged := jwtEncode(`{"alg":"none","typ":"JWT"}`) + "." + payload + "."

	_, err := exchangeInjected(t, provider, client, forged)
	if err == nil {
		t.Fatal("alg=none token accepted")
	}
	if !strings.Contains(err.Error(), "unsupported algorithm") {
		t.Errorf("error = %v, want unsupported algorithm", err)
	}
}

func TestOIDCClientRejectsDiscoveryIssuerMismatch(t *testing.T) {
	provider := oidctest.NewProvider()
	provider.Serve()
	defer provider.Close()

	// A hostile discovery document relocating the issuer.
	rogue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issuer":"https://elsewhere.example","authorization_endpoint":"` +
			provider.Issuer() + `/authorize","token_endpoint":"` + provider.Issuer() +
			`/token","jwks_uri":"` + provider.Issuer() + `/jwks"}`))
	}))
	defer rogue.Close()

	client := authn.NewOIDCClient(rogue.URL, oidctest.ClientID, oidctest.ClientSecret, "https://api/cb")
	if _, err := client.AuthorizeURL(context.Background(), "https://api/cb", "s"); err == nil || !strings.Contains(err.Error(), "issuer mismatch") {
		t.Errorf("discovery issuer mismatch = %v, want issuer mismatch error", err)
	}
}

func TestOIDCClientEmailVerifiedStringFormAccepted(t *testing.T) {
	// Interop: some providers quote email_verified as the string "true".
	// The client must accept it. Enforcement of *unverified* email is
	// service policy, tested at the service layer.
	provider := oidctest.NewProvider()
	provider.Serve()
	defer provider.Close()
	client := provider.NewClient("https://api/cb")

	token, err := provider.SignIDTokenWith(map[string]any{
		"iss": provider.Issuer(), "sub": "s", "aud": oidctest.ClientID,
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"email": "a@b.co", "email_verified": "true",
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	claims, err := exchangeInjected(t, provider, client, token)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if !claims.EmailVerified {
		t.Error(`email_verified:"true" (string form) must be accepted as verified`)
	}
}

func TestOIDCClientDiscoveryFailure(t *testing.T) {
	provider := oidctest.NewProvider()
	provider.Serve()
	issuer := provider.Issuer()
	provider.Close() // provider gone: discovery must fail cleanly

	client := authn.NewOIDCClient(issuer, oidctest.ClientID, oidctest.ClientSecret, "https://api/cb")
	if _, err := client.AuthorizeURL(context.Background(), "https://api/cb", "s"); err == nil {
		t.Error("authorize URL against a dead provider succeeded")
	}
}
