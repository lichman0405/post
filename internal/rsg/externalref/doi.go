package externalref

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/lichman0405/post/internal/domain"
)

// DoiFetcher is the V1 upstream metadata adapter: DOI content
// negotiation. GET https://doi.org/{doi} with Accept
// application/vnd.citationstyles.csl+json resolves ANY DOI through the
// handle system to its registering agency (CrossRef, DataCite, ...) —
// agency-independent, no API key, one endpoint (docs/19 §5: "V1 可先支持
// 手动 refresh + DOI metadata adapter"). The answer is a CSL-JSON
// document, a JSON object whose fields (title, version, URL, publisher,
// issued, ...) become the snapshot metadata.
//
// The fetch runs on the SSRF-guarded client (docs/23 §7, docs/54 #5):
// https-only, no private address space, every redirect hop re-checked.
// Fetched content is untrusted data, never instructions (docs/23 §8):
// the document is parsed strictly, size-capped, and stored as-is — it is
// a record of what upstream said, not an input to any interpreter.
type DoiFetcher struct {
	// baseURL is the resolver root (default https://doi.org); tests
	// point it at a local server.
	baseURL string
	// client is the guarded HTTP client; tests inject a transport.
	client *http.Client
	// maxBytes caps one metadata document (default maxMetadataBytes).
	maxBytes int64
}

// DoiOption configures a fetcher (tests and future wiring).
type DoiOption func(*DoiFetcher)

// WithDoiBaseURL overrides the resolver root (must be an https URL; the
// guard enforces the scheme at fetch time, this is the default value).
func WithDoiBaseURL(u string) DoiOption { return func(f *DoiFetcher) { f.baseURL = u } }

// WithDoiClient injects the HTTP client (default: the SSRF-guarded
// client built by the guard). A caller that replaces it takes over the
// URL policy responsibility for its own transport.
func WithDoiClient(c *http.Client) DoiOption { return func(f *DoiFetcher) { f.client = c } }

// WithDoiMaxBytes overrides the metadata size cap.
func WithDoiMaxBytes(n int64) DoiOption { return func(f *DoiFetcher) { f.maxBytes = n } }

// DefaultDoiBaseURL is the DOI resolver root (the handle system's
// content-negotiation endpoint).
const DefaultDoiBaseURL = "https://doi.org"

// NewDoiFetcher builds the V1 adapter. The zero-option fetcher uses the
// production defaults: doi.org, the SSRF-guarded client with
// DefaultFetchTimeout, and the 1 MiB metadata cap.
func NewDoiFetcher(opts ...DoiOption) *DoiFetcher {
	f := &DoiFetcher{
		baseURL:  DefaultDoiBaseURL,
		maxBytes: maxMetadataBytes,
	}
	for _, o := range opts {
		o(f)
	}
	if f.client == nil {
		f.client = NewGuard().NewFetchClient(DefaultFetchTimeout)
	}
	return f
}

// Fetch implements Fetcher. The identifier must be DOI-shaped; the
// canonical URL is ignored (the DOI IS the resolution path). The answer
// is re-marshaled so Metadata is canonical JSON (deterministic key
// order) and UpstreamVersion carries the CSL-JSON version field when
// upstream reports one.
func (f *DoiFetcher) Fetch(ctx context.Context, ref Ref) (UpstreamMetadata, error) {
	doi, err := resolveDOI(ref.ExternalIdentifier)
	if err != nil {
		return UpstreamMetadata{}, err
	}
	base, err := url.Parse(strings.TrimRight(f.baseURL, "/") + "/")
	if err != nil {
		return UpstreamMetadata{}, errf(ErrNotResolvable, "DOI resolver base %q is not a URL: %v", f.baseURL, err)
	}
	target := base.ResolveReference(&url.URL{Path: doi})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return UpstreamMetadata{}, errf(ErrNotResolvable, "building DOI request: %v", err)
	}
	req.Header.Set("Accept", "application/vnd.citationstyles.csl+json")

	resp, err := f.client.Do(req)
	if err != nil {
		return UpstreamMetadata{}, errf(ErrUpstreamUnavailable, "fetching %s: %v", redact(target), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return UpstreamMetadata{}, errf(ErrUpstreamUnavailable, "fetching %s: upstream answered %d", redact(target), resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBytes+1))
	if err != nil {
		return UpstreamMetadata{}, errf(ErrUpstreamUnavailable, "reading %s: %v", redact(target), err)
	}
	if int64(len(body)) > f.maxBytes {
		return UpstreamMetadata{}, errf(ErrInvalidUpstream, "metadata document exceeds the %d-byte cap", f.maxBytes)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return UpstreamMetadata{}, errf(ErrInvalidUpstream, "upstream answer is not a JSON document: %v", err)
	}
	canonical, err := json.Marshal(doc)
	if err != nil {
		return UpstreamMetadata{}, errf(ErrInvalidUpstream, "re-encoding upstream document: %v", err)
	}
	version, _ := doc["version"].(string)
	return UpstreamMetadata{UpstreamVersion: strings.TrimSpace(version), Metadata: canonical}, nil
}

// resolveDOI extracts and normalizes a DOI name from an identifier. The
// accepted spellings are the ones users actually paste — the bare DOI,
// the doi: scheme, or a doi.org URL; normalization (case-fold, prefix
// strip) is domain.NormalizeExternalIdentifier's rule, so an identity
// and its fetch agree on the same DOI form.
func resolveDOI(identifier string) (string, error) {
	normalized, err := domain.NormalizeExternalIdentifier(identifier)
	if err != nil {
		if errors.Is(err, domain.ErrIdentifierEmpty) {
			return "", errf(ErrNotResolvable, "external_identifier is empty")
		}
		return "", errf(ErrNotResolvable, "normalizing %q: %v", identifier, err)
	}
	if !doiNameShape.MatchString(normalized) {
		return "", errf(ErrNotResolvable, "identifier %q is not DOI-shaped (the V1 adapter resolves DOIs; other source types await their adapters)", identifier)
	}
	return normalized, nil
}

// doiNameShape is the minimal DOI name form: 10.<registrant>/<suffix>.
var doiNameShape = regexp.MustCompile(`^10\.\d{4,9}/.+$`)

// redact strips userinfo from a URL for error messages (credentials must
// never reach a log line, docs/23 §10).
func redact(u *url.URL) string {
	c := *u
	c.User = nil
	return c.String()
}

var _ Fetcher = (*DoiFetcher)(nil)
