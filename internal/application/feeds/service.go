package feeds

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Config is the feed surface's configuration.
type Config struct {
	// BaseURL is the public origin the rendered links are built from —
	// scheme://host[:port], no path (the configured web origin: the pages a
	// feed entry links to are the web app's). It is validated at wiring
	// time: a feed whose links cannot be built must not be served at all,
	// and a broken base is a deployment mistake, not a request error.
	//
	// It is a CONFIGURED origin and never the request's Host: a public
	// document that echoed a caller-supplied host would hand every reader
	// whatever URL that caller chose.
	BaseURL string
	// MaxEntries bounds one feed's length; 0 means DefaultMaxEntries.
	MaxEntries int
}

// Service is the feed use case: resolve a target's state, decide whether a
// public feed exists, and assemble it.
type Service struct {
	reader  Reader
	baseURL string
	max     int
}

// NewService wires the service over a reader. It fails when BaseURL is not
// a usable public origin — the one configuration error this surface cannot
// serve around, because every URL in a feed document is absolute (Atom's
// link/@href is an IRI, and RSS's link is a URL).
func NewService(reader Reader, cfg Config) (*Service, error) {
	if reader == nil {
		return nil, fmt.Errorf("%w: no reader wired", ErrConfig)
	}
	base, err := validateBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	return &Service{reader: reader, baseURL: base, max: Options{MaxEntries: cfg.MaxEntries}.maxEntries()}, nil
}

// validateBaseURL accepts scheme://host[:port] with an optional trailing
// slash and nothing else, and returns the form the link builders append
// paths to. Everything a browser would treat as a different resource —
// a path, a query, a fragment, userinfo — is refused rather than trimmed:
// silently dropping "/app" would mint links pointing somewhere the operator
// did not ask for.
func validateBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("%w: POST_WEB_ORIGIN is empty", ErrConfig)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrConfig, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%w: scheme must be http or https", ErrConfig)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%w: host is required", ErrConfig)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: userinfo, query and fragment are not allowed", ErrConfig)
	}
	if path := strings.TrimSuffix(u.Path, "/"); path != "" {
		return "", fmt.Errorf("%w: a path is not allowed (got %q)", ErrConfig, u.Path)
	}
	return u.Scheme + "://" + u.Host, nil
}

// BaseURL is the origin the rendered links are built from. It is exposed so
// a caller that has to construct a feed URL (the web app's
// rel="alternate" link) can ask this service rather than keep a second copy
// of the rule.
func (s *Service) BaseURL() string { return s.baseURL }

// Feed resolves one target and answers its feed, or ErrNotFound when there
// is no public feed to serve (see BuildFeed for the three states that
// answer it), ErrValidation for input that could never name a stored row,
// and ErrStore when the read failed.
//
// The READER — not this method — is what limits: the state read is asked
// for `max` entries per entry kind, and it answers with the newest `max`
// rows a public feed may RENDER, so the window is always spent on entries
// this feed can carry (a bounded read counted in raw rows would let a burst
// of private versions suppress a public feed entirely — see the header of
// internal/persistence/queries/feeds.sql). BuildFeed then sorts and caps.
func (s *Service) Feed(ctx context.Context, target Target) (Feed, error) {
	parsed, err := ParseTarget(string(target.Kind), target.ID)
	if err != nil {
		return Feed{}, err
	}
	state, err := s.reader.LoadFeedState(ctx, parsed, s.max)
	if err != nil {
		return Feed{}, fmt.Errorf("%w: read %s feed state: %v", ErrStore, parsed.Kind, err)
	}
	feed, ok := BuildFeed(parsed, state, Options{BaseURL: s.baseURL, MaxEntries: s.max})
	if !ok {
		return Feed{}, ErrNotFound
	}
	return feed, nil
}

// Document renders one target's feed in one format: the whole answer a feed
// client asked for, in one call.
//
// The format is checked BEFORE the read: a request this service cannot
// serialize is answerable without touching storage, and the transport
// answers it 400 rather than 404 — the caller's own request is wrong, and
// the target's existence is what must not be disclosed to settle that.
func (s *Service) Document(ctx context.Context, target Target, format Format) ([]byte, error) {
	if !format.Valid() {
		return nil, fmt.Errorf("%w: unknown feed format %q", ErrValidation, format)
	}
	feed, err := s.Feed(ctx, target)
	if err != nil {
		return nil, err
	}
	return Render(feed, format)
}
