package authn

// Error codes on the wire (docs/45). Every authentication failure maps to
// one of these; the handler renders them inside the standard error
// envelope. Codes starting AUTH_ are the ones docs/45 names first-class.
const (
	// CodeInvalidCredentials is returned for an unknown account OR a wrong
	// password — deliberately one code for both (enumeration protection).
	CodeInvalidCredentials = "AUTH_INVALID_CREDENTIALS"
	// CodeUnauthenticated is returned when a protected write arrives with
	// no (or an expired/revoked) session.
	CodeUnauthenticated = "AUTH_UNAUTHENTICATED"
	// CodeEmailTaken is returned by signup when the email is already
	// registered. Disclosure on signup is the industry-standard tradeoff
	// (login stays enumeration-safe); see T0101 RESULT risks.
	CodeEmailTaken = "EMAIL_ALREADY_REGISTERED"
	// CodeRateLimited is returned when the login rate limit trips.
	CodeRateLimited = "RATE_LIMITED"
	// CodeCSRFFailed is returned when a state-changing request carries a
	// missing or mismatched X-CSRF-Token (or a cross-site Origin).
	CodeCSRFFailed = "CSRF_FAILED"
	// CodeValidationFailed is returned for malformed input (bad email
	// shape, short password, unknown fields are ignored per contract).
	CodeValidationFailed = "VALIDATION_FAILED"
	// CodeOIDCNotConfigured: the deployment has no OIDC provider.
	CodeOIDCNotConfigured = "OIDC_NOT_CONFIGURED"
	// CodeOIDCStateMismatch: the callback state did not match the flow.
	CodeOIDCStateMismatch = "OIDC_STATE_MISMATCH"
	// CodeOIDCEmailNotVerified: the provider did not assert a verified
	// email and POST refuses to create an identity from an unverified one.
	CodeOIDCEmailNotVerified = "OIDC_EMAIL_NOT_VERIFIED"
	// CodeOIDCProviderFailed: the provider exchange/verification failed
	// (no detail about which step leaks to the client).
	CodeOIDCProviderFailed = "OIDC_PROVIDER_FAILED"
	// CodeServiceUnavailable: a required dependency (session store, rate
	// limiter) failed — fail closed, never guess.
	CodeServiceUnavailable = "SERVICE_UNAVAILABLE"
)
