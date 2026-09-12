package authn

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OIDC authorization-code client implemented on the standard library only
// (T0101 L1: no third-party OIDC dependency in the V1 core). It performs
// discovery, code exchange and RS256 id_token verification itself. The
// verification rules are the OIDC Core 1.0 §3.1.3.7 subset that matters
// here: signature over the exact bytes received, issuer/audience match,
// time validity, and (enforced at the service layer) verified email.
//
// It deliberately accepts ONLY RS256: algorithm confusion (alg=none,
// HS256 with the public key as secret) is the classic hand-rolled JWT
// failure, and a fixed single algorithm closes it by construction.

// HTTPClient is the OIDC client's HTTP dependency, injectable for tests.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// OIDCClient is the real OIDCProvider implementation.
type OIDCClient struct {
	http     HTTPClient
	issuer   string
	clientID string
	secret   string
	redirect string
}

// NewOIDCClient builds a client for one provider. The client performs
// discovery lazily on first use, so startup never blocks on the provider
// being reachable (the API starts even when the IdP is down, like the
// database — readiness semantics, T0006).
func NewOIDCClient(issuer, clientID, clientSecret, redirectURI string) *OIDCClient {
	return &OIDCClient{
		http:     http.DefaultClient,
		issuer:   strings.TrimSuffix(issuer, "/"),
		clientID: clientID,
		secret:   clientSecret,
		redirect: redirectURI,
	}
}

// oidcDiscovery is the subset of the discovery document the client uses.
type oidcDiscovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

func (c *OIDCClient) discover(ctx context.Context) (*oidcDiscovery, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("authn: oidc discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("authn: oidc discovery: provider answered %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("authn: oidc discovery: %w", err)
	}
	var doc oidcDiscovery
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("authn: oidc discovery: %w", err)
	}
	// A discovery document may not relocate the issuer (mixing-and-matching
	// attack: a malicious IdP claiming a different issuer's endpoints).
	if doc.Issuer != "" && doc.Issuer != c.issuer {
		return nil, fmt.Errorf("authn: oidc discovery: issuer mismatch (%s != %s)", doc.Issuer, c.issuer)
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" || doc.JWKSURI == "" {
		return nil, errors.New("authn: oidc discovery: document missing required endpoints")
	}
	return &doc, nil
}

// AuthorizeURL builds the provider authorization URL for one flow. The
// redirect_uri must be the API's own callback endpoint: the transport
// derives it from the request (callbackURL), because the API's externally
// visible origin is unknowable at startup.
func (c *OIDCClient) AuthorizeURL(ctx context.Context, callbackURL, state string) (string, error) {
	doc, err := c.discover(ctx)
	if err != nil {
		return "", err
	}
	redirect := callbackURL
	if redirect == "" {
		redirect = c.redirect
	}
	u, err := url.Parse(doc.AuthorizationEndpoint)
	if err != nil {
		return "", fmt.Errorf("authn: oidc authorize: %w", err)
	}
	q := u.Query()
	q.Set("client_id", c.clientID)
	q.Set("redirect_uri", redirect)
	q.Set("response_type", "code")
	q.Set("scope", "openid email profile")
	q.Set("state", state)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// ExchangeCode completes the authorization code flow and returns verified
// claims.
func (c *OIDCClient) ExchangeCode(ctx context.Context, code, redirectURI string) (OIDCClaims, error) {
	doc, err := c.discover(ctx)
	if err != nil {
		return OIDCClaims{}, err
	}
	if redirectURI == "" {
		redirectURI = c.redirect
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {c.clientID},
		"client_secret": {c.secret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, doc.TokenEndpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return OIDCClaims{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return OIDCClaims{}, fmt.Errorf("authn: oidc token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return OIDCClaims{}, fmt.Errorf("authn: oidc token exchange: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return OIDCClaims{}, fmt.Errorf("authn: oidc token exchange: provider answered %d", resp.StatusCode)
	}
	var token struct {
		IDToken     string `json:"id_token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &token); err != nil || token.IDToken == "" {
		return OIDCClaims{}, errors.New("authn: oidc token exchange: no id_token in response")
	}

	claims, err := c.verifyIDToken(ctx, token.IDToken, doc)
	if err != nil {
		return OIDCClaims{}, err
	}
	// Some providers (email in userinfo only) require the extra call.
	if claims.Email == "" && doc.UserinfoEndpoint != "" && token.AccessToken != "" {
		if extra, err := c.fetchUserinfo(ctx, doc.UserinfoEndpoint, token.AccessToken); err == nil {
			if claims.Email == "" {
				claims.Email = extra.email
				claims.EmailVerified = extra.emailVerified
			}
			if claims.PreferredUsername == "" {
				claims.PreferredUsername = extra.preferredUsername
			}
			if claims.Name == "" {
				claims.Name = extra.name
			}
		}
	}
	return claims, nil
}

// idTokenClaims is the verified subset of the id_token the client exposes.
type idTokenClaims struct {
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"`
	Audience  any    `json:"aud"` // string or []string per spec
	ExpiresAt int64  `json:"exp"`
	IssuedAt  int64  `json:"iat"`
	NotBefore int64  `json:"nbf"`
	Email     string `json:"email"`
	// email_verified: some providers send bool, some send the string
	// "true". Both are accepted, anything else is "unverified".
	EmailVerified     any    `json:"email_verified"`
	PreferredUsername string `json:"preferred_username"`
	Name              string `json:"name"`
}

func (c *OIDCClient) verifyIDToken(ctx context.Context, raw string, doc *oidcDiscovery) (OIDCClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return OIDCClaims{}, errors.New("authn: oidc id_token: not a JWS compact serialization")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return OIDCClaims{}, fmt.Errorf("authn: oidc id_token: header: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return OIDCClaims{}, fmt.Errorf("authn: oidc id_token: header: %w", err)
	}
	if header.Alg != "RS256" {
		return OIDCClaims{}, fmt.Errorf("authn: oidc id_token: unsupported algorithm %q", header.Alg)
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return OIDCClaims{}, fmt.Errorf("authn: oidc id_token: payload: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return OIDCClaims{}, fmt.Errorf("authn: oidc id_token: signature: %w", err)
	}
	signed := []byte(parts[0] + "." + parts[1])

	key, err := c.jwksKey(ctx, doc.JWKSURI, header.Kid)
	if err != nil {
		return OIDCClaims{}, err
	}
	digest := sha256.Sum256(signed)
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return OIDCClaims{}, errors.New("authn: oidc id_token: signature verification failed")
	}

	var claims idTokenClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return OIDCClaims{}, fmt.Errorf("authn: oidc id_token: claims: %w", err)
	}
	if claims.Issuer != c.issuer {
		return OIDCClaims{}, fmt.Errorf("authn: oidc id_token: issuer %q != %q", claims.Issuer, c.issuer)
	}
	if !audienceContains(claims.Audience, c.clientID) {
		return OIDCClaims{}, errors.New("authn: oidc id_token: audience does not include the client id")
	}
	now := time.Now().Unix()
	// OIDC Core requires exp on id_tokens: a token without one would never
	// expire. Missing exp is a hard rejection, not "skip the check".
	if claims.ExpiresAt == 0 {
		return OIDCClaims{}, errors.New("authn: oidc id_token: missing exp")
	}
	if now >= claims.ExpiresAt {
		return OIDCClaims{}, errors.New("authn: oidc id_token: expired")
	}
	if claims.NotBefore != 0 && now < claims.NotBefore {
		return OIDCClaims{}, errors.New("authn: oidc id_token: not yet valid")
	}
	// iat far in the future = clock attack; reject beyond a small skew.
	if claims.IssuedAt > now+300 {
		return OIDCClaims{}, errors.New("authn: oidc id_token: issued in the future")
	}
	if claims.Subject == "" {
		return OIDCClaims{}, errors.New("authn: oidc id_token: missing sub")
	}
	return OIDCClaims{
		Subject:           claims.Subject,
		Email:             domainLowerEmail(claims.Email),
		EmailVerified:     emailVerifiedTrue(claims.EmailVerified),
		PreferredUsername: claims.PreferredUsername,
		Name:              claims.Name,
	}, nil
}

func audienceContains(aud any, want string) bool {
	switch v := aud.(type) {
	case string:
		return v == want
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

// emailVerifiedTrue accepts bool true and the JSON string "true"
// (interoperability with providers that quote it).
func emailVerifiedTrue(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(t, "true")
	}
	return false
}

func domainLowerEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// jwksKey fetches the provider's signing keys and returns the RSA public
// key matching kid. The full JWKS is fetched per verification (the API is
// not a high-frequency IdP client; a cache is an optimization, not a
// correctness need).
func (c *OIDCClient) jwksKey(ctx context.Context, jwksURI, kid string) (*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURI, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("authn: oidc jwks: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("authn: oidc jwks: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("authn: oidc jwks: provider answered %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			Alg string `json:"alg"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("authn: oidc jwks: %w", err)
	}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || (kid != "" && k.Kid != kid) {
			continue
		}
		if k.Alg != "" && k.Alg != "RS256" {
			continue
		}
		nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil || len(eBytes) == 0 {
			continue
		}
		e := 0
		for _, b := range eBytes {
			e = e<<8 | int(b)
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}, nil
	}
	if kid == "" {
		return nil, errors.New("authn: oidc jwks: no usable RSA key")
	}
	return nil, fmt.Errorf("authn: oidc jwks: no key with kid %q", kid)
}

type userinfoClaims struct {
	email             string
	emailVerified     bool
	preferredUsername string
	name              string
}

// fetchUserinfo pulls the userinfo claims with the access token (the
// email-missing fallback only; identity still comes from the id_token).
func (c *OIDCClient) fetchUserinfo(ctx context.Context, endpoint, accessToken string) (userinfoClaims, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return userinfoClaims{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := c.http.Do(req)
	if err != nil {
		return userinfoClaims{}, fmt.Errorf("authn: oidc userinfo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return userinfoClaims{}, fmt.Errorf("authn: oidc userinfo: provider answered %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return userinfoClaims{}, err
	}
	var raw struct {
		Email             string `json:"email"`
		EmailVerified     any    `json:"email_verified"`
		PreferredUsername string `json:"preferred_username"`
		Name              string `json:"name"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return userinfoClaims{}, err
	}
	return userinfoClaims{
		email:             domainLowerEmail(raw.Email),
		emailVerified:     emailVerifiedTrue(raw.EmailVerified),
		preferredUsername: raw.PreferredUsername,
		name:              raw.Name,
	}, nil
}
