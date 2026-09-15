package events

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// WebhookEndpoint is one registered delivery target (T1006). The registry
// row owns url/secret/filters/enabled; the delivery log
// (webhook_deliveries) references it by id. Secret is only ever non-empty
// on the Create/RegenerateSecret return path — every read path renders
// the model without it (the application service strips it before any
// payload leaves the process; the store keeps it out of read queries by
// construction).
type WebhookEndpoint struct {
	ID                  string
	UserID              string
	URL                 string
	Secret              string
	EventFilters        []string
	Enabled             bool
	ConsecutiveFailures int
	DisabledAt          *time.Time
	CreatedAt           time.Time
}

// Policy constants for webhook delivery (L1 decisions, documented here as
// the single place they live):
const (
	// DefaultSecretBytes is the generated secret length (32 random bytes,
	// hex-encoded to 64 characters — 256 bits of HMAC key material).
	DefaultSecretBytes = 32
	// MaxEndpointsPerUser bounds the registry per owner: a hostile account
	// must not be able to grow the fan-out join unboundedly.
	MaxEndpointsPerUser = 20
	// MaxWebhookURLLength bounds the stored URL (generous but finite).
	MaxWebhookURLLength = 2048
	// MaxEventFilters bounds the filter list per endpoint.
	MaxEventFilters = 100
	// MaxEventFilterLength bounds one filter (the canonical vocabulary's
	// longest name is far shorter).
	MaxEventFilterLength = 100
	// DefaultConsecutiveFailureLimit is the disable policy: after this
	// many consecutive failed deliveries the deliverer disables the
	// endpoint (enabled=false, disabled_at=now) and stops hammering it.
	// The owner may re-enable. GitHub-shaped (their threshold is 20).
	DefaultConsecutiveFailureLimit = 10
)

// Delivery statuses (webhook_deliveries.status). The vocabulary is closed:
// the store and deliverer only ever write these values, and the tests
// assert the set.
const (
	DeliveryPending   = "pending"   // awaiting first attempt or a retry
	DeliveryDelivered = "delivered" // a 2xx arrived; attempts stop
	DeliveryFailed    = "failed"    // terminal: a 4xx said the request is permanently bad
	DeliveryCancelled = "cancelled" // the endpoint was deleted before delivery
)

// Delivery is one webhook_deliveries row as the delivery-log API renders
// it — the whole point of the log (docs/22 §9): what was sent, where,
// how many attempts, and why it failed when it did.
type Delivery struct {
	ID            string
	EndpointID    string
	EventID       string
	EventType     string
	EndpointURL   string
	Status        string
	ResponseCode  *int
	Attempts      int
	LastAttemptAt *time.Time
	NextRetryAt   *time.Time
	LastError     *string
	DeliveredAt   *time.Time
	CreatedAt     time.Time
}

// GenerateSecret returns a fresh DefaultSecretBytes HMAC secret as
// lowercase hex (64 chars). crypto/rand — the same source as newUUID.
func GenerateSecret() (string, error) {
	var b [DefaultSecretBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("events: webhook secret: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ValidateWebhookURL checks the delivery target's shape at registration
// and update time: an absolute http(s) URL with a host and no embedded
// userinfo. Credentials in the URL are refused because the URL is written
// verbatim into the delivery log — a basic-auth URL would leak its
// password into the log. Reaching private addresses is a separate,
// delivery-time check (the SSRF dial guard), not this one: DNS answers
// change, and only the dial can see the address actually used.
func ValidateWebhookURL(raw string) error {
	if len(raw) > MaxWebhookURLLength {
		return fmt.Errorf("events: webhook url too long (max %d characters)", MaxWebhookURLLength)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("events: webhook url is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("events: webhook url scheme must be http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("events: webhook url must include a host")
	}
	if u.User != nil {
		return fmt.Errorf("events: webhook url must not embed credentials")
	}
	return nil
}

// ValidateEventFilters checks one endpoint's filter list shape. Filters
// match event types by exact string equality at fan-out (the canonical
// vocabulary in specs/events/event-types.yaml is not duplicated in code —
// a duplicated list drifts); an unknown type simply never matches. The
// charset bound keeps hostile input from turning the fan-out comparison
// into an attack surface.
func ValidateEventFilters(filters []string) error {
	if len(filters) > MaxEventFilters {
		return fmt.Errorf("events: too many event filters (max %d)", MaxEventFilters)
	}
	for _, f := range filters {
		if f == "" || len(f) > MaxEventFilterLength {
			return fmt.Errorf("events: event filter must be 1-%d characters", MaxEventFilterLength)
		}
		if strings.Trim(f, "abcdefghijklmnopqrstuvwxyz0123456789._") != "" {
			return fmt.Errorf("events: event filter %q contains invalid characters (a-z, 0-9, dot, underscore)", f)
		}
	}
	return nil
}
