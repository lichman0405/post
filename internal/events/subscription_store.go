package events

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/knowledgepublish"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rights"
)

// SubscriptionStore is the subscription persistence: the CRUD the
// application service drives, the audience resolution the whole pipeline
// shares, and the fan-out's delivery writes. Raw SQL like the outbox
// dispatcher and the webhook store — the subscription tables belong to the
// events domain, whose outbox and fan-out they serve.
//
// It is the ONE place that answers "how is this user related to this
// target right now" (TargetAudience). Both the subscribe path and the
// fan-out call it, so the rule that decides whether a follow may be
// created and the rule that decides whether an event may be delivered
// cannot drift apart — a second copy of that rule is exactly how a
// revoked membership keeps receiving private events.
type SubscriptionStore struct {
	pool *pgxpool.Pool
}

// NewSubscriptionStore builds the store on pool. The pool may be lazy
// (persistence.OpenLazy): like the dispatcher, the fan-out retries while
// PostgreSQL is down.
func NewSubscriptionStore(pool *pgxpool.Pool) *SubscriptionStore {
	return &SubscriptionStore{pool: pool}
}

// subscriptionCols is the subscription row as every read returns it.
const subscriptionCols = `id, user_id, target_type, target_id, event_filters, channels, created_at, updated_at, deleted_at`

// deliveryCols is the delivery row as every read returns it.
const deliveryCols = `id, subscription_id, user_id, event_id, channel, event_type, target_type, target_id, status, created_at, delivered_at, cancelled_at, read_at`

// CreateSubscription registers one live subscription for userID.
//
// Two rules are enforced here rather than by the caller:
//
//   - the per-user limit, checked under the user row's lock so two
//     concurrent creates cannot both pass the count (the webhook
//     registry's pattern);
//   - one live subscription per (user, target): the partial unique index
//     decides, and the violation is translated to ErrSubscriptionExists
//     rather than surfaced as a constraint name. An ENDED subscription
//     does not stand in the way — re-following after unsubscribing inserts
//     a new row and leaves the ended one, and its deliveries, in place.
func (s *SubscriptionStore) CreateSubscription(ctx context.Context, userID string, target Target, filters, channels []string) (Subscription, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Subscription{}, fmt.Errorf("events: create subscription: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var locked string
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&locked); err != nil {
		return Subscription{}, fmt.Errorf("events: create subscription: lock owner: %w", err)
	}
	var live int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM subscriptions WHERE user_id = $1 AND deleted_at IS NULL`, userID).Scan(&live); err != nil {
		return Subscription{}, fmt.Errorf("events: create subscription: count: %w", err)
	}
	if live >= MaxSubscriptionsPerUser {
		return Subscription{}, ErrSubscriptionLimit
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO subscriptions (user_id, target_type, target_id, event_filters, channels)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+subscriptionCols,
		userID, target.Type, target.ID, emptyIfNil(filters), emptyIfNil(channels))
	sub, err := scanSubscription(row)
	if err != nil {
		if isUniqueViolation(err, "subscriptions_live_uniq") {
			return Subscription{}, ErrSubscriptionExists
		}
		return Subscription{}, fmt.Errorf("events: create subscription: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Subscription{}, fmt.Errorf("events: create subscription: commit: %w", err)
	}
	return sub, nil
}

// ListSubscriptions returns the user's live subscriptions, newest first.
// A non-empty targetType filters to one target (the watch-state read a
// surface makes before rendering a follow control); targetID alone
// filters to one target by id.
func (s *SubscriptionStore) ListSubscriptions(ctx context.Context, userID, targetType, targetID string) ([]Subscription, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+subscriptionCols+`
		FROM subscriptions
		WHERE user_id = $1 AND deleted_at IS NULL
		  AND ($2 = '' OR target_type = $2)
		  AND ($3 = '' OR target_id = $3)
		ORDER BY created_at DESC, id DESC`, userID, targetType, targetID)
	if err != nil {
		return nil, fmt.Errorf("events: list subscriptions: %w", err)
	}
	defer rows.Close()
	out := []Subscription{}
	for rows.Next() {
		sub, err := scanSubscription(rows)
		if err != nil {
			return nil, fmt.Errorf("events: list subscriptions: scan: %w", err)
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// GetSubscription returns one live subscription owned by userID, or
// ErrSubscriptionNotFound.
func (s *SubscriptionStore) GetSubscription(ctx context.Context, userID, id string) (Subscription, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+subscriptionCols+`
		FROM subscriptions
		WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`, id, userID)
	return withSubscriptionNotFound(scanSubscription(row))
}

// UpdateSubscription replaces the subscription's event filters and
// channels (a full replacement, like the webhook endpoint's filters) and
// stamps updated_at, or answers ErrSubscriptionNotFound. The target is not
// editable: changing what you follow is a different subscription, and
// letting the target move would re-point deliveries already fanned out
// under the old one.
func (s *SubscriptionStore) UpdateSubscription(ctx context.Context, userID, id string, filters, channels []string) (Subscription, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE subscriptions
		SET event_filters = $3, channels = $4, updated_at = now()
		WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL
		RETURNING `+subscriptionCols, id, userID, emptyIfNil(filters), emptyIfNil(channels))
	return withSubscriptionNotFound(scanSubscription(row))
}

// DeleteSubscription unsubscribes: the row is stamped deleted_at (nothing
// disappears — the deliveries it produced keep naming it, and the target
// may be followed again as a new row) and every delivery still in flight
// is cancelled IN THE SAME transaction.
//
// Cancelling them is the "取消订阅生效" half of this task's acceptance: a
// channel that delivers later (email) must not send a notification the
// owner has already unsubscribed from, so the unsubscribe has to reach
// rows that were fanned out before it — an unsubscribe that only stopped
// future fan-out would let a queued email arrive after it. Rows already
// delivered are untouched: they were delivered.
func (s *SubscriptionStore) DeleteSubscription(ctx context.Context, userID, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("events: delete subscription: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var deleted string
	err = tx.QueryRow(ctx, `
		UPDATE subscriptions
		SET deleted_at = now(), updated_at = now()
		WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL
		RETURNING id`, id, userID).Scan(&deleted)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSubscriptionNotFound
		}
		return fmt.Errorf("events: delete subscription: %w", err)
	}
	if _, err := tx.Exec(ctx, cancelPendingDeliveries, deleted); err != nil {
		return fmt.Errorf("events: delete subscription: cancel deliveries: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("events: delete subscription: commit: %w", err)
	}
	return nil
}

// cancelPendingDeliveries withdraws every delivery of one subscription
// that has not been delivered yet.
const cancelPendingDeliveries = `
UPDATE subscription_deliveries
SET status = 'cancelled', cancelled_at = now()
WHERE subscription_id = $1 AND status = 'pending'`

// TargetAudience answers how userID is related to target RIGHT NOW — the
// rule the subscribe path and the fan-out share (see the type comment).
// It is resolved from live rows on every call: a project's visibility, a
// project membership, an organization's deactivation, an organization
// membership that ended, a disabled account. Nothing is remembered from
// subscribe time, which is what makes a permission change take effect at
// the next event (T1002 acceptance: private target 权限变化后不继续泄漏).
//
// The rule per target type, all in the fail-closed direction — an
// unresolvable target (unknown id, a row that cannot be read) is
// AudienceNone, never a guess:
//
//	project      member of it; else public if its visibility is public
//	asset        the audience of the project the asset originates from
//	             (the project read gate is what the asset page itself runs)
//	knowledge    member of the project owning the object; else the
//	             publication's own audience — knowledgepublish.AudienceFor,
//	             which is what "发布不等于公开" means (see knowledgeAudience)
//	user         the person themselves; else public (a live profile is
//	             public — a disabled account is not)
//	organization an active member; else public unless deactivated
func (s *SubscriptionStore) TargetAudience(ctx context.Context, db DBTX, target Target, userID string) (AudienceLevel, error) {
	if target.Type == TargetTypeKnowledge {
		// A publication's audience is not "the project's visibility" and it
		// is not a CASE expression either: it is the three-axis rule the
		// publish side and the read path already share, so it is asked
		// rather than restated (see knowledgeAudience).
		return knowledgeAudience(ctx, db, target, userID)
	}
	query, ok := audienceQueries[target.Type]
	if !ok {
		return AudienceNone, fmt.Errorf("%w: unknown target type %q", ErrTargetShape, target.Type)
	}
	// The organization rule is the only one of these that reads a calendar
	// day, and the day is the caller's: it is resolved here, in UTC, so that
	// neither the process's clock zone nor the database session's can decide
	// who is a member (domain.AffiliationDay — one convention, one place).
	args := []any{target.ID, userID}
	if target.Type == TargetTypeOrganization {
		args = append(args, domain.AffiliationDayText(time.Now()))
	}
	var level string
	err := db.QueryRow(ctx, query, args...).Scan(&level)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The target does not exist (or is not the shape the query
			// casts): no relationship, no delivery.
			return AudienceNone, nil
		}
		return AudienceNone, fmt.Errorf("events: resolve audience for %s %s: %w", target.Type, target.ID, err)
	}
	return AudienceLevel(level), nil
}

// audienceQueries resolves one user's level on one target, per target
// type. Each returns exactly one row when the target exists.
var audienceQueries = map[string]string{
	// A project: membership, then public visibility. project_memberships
	// is the same table the project read gate reads (docs/12 §2: a private
	// project is its members').
	TargetTypeProject: `
SELECT CASE
         WHEN EXISTS (SELECT 1 FROM project_memberships m
                      WHERE m.project_id = p.id AND m.user_id = $2::uuid) THEN 'member'
         WHEN p.visibility = 'public' THEN 'public'
         ELSE 'none'
       END
FROM projects p
WHERE p.id = $1::uuid`,

	// An asset: its origin project decides, because that project's read
	// gate is what the asset page itself runs (cmd/api/assetshttp: a
	// caller who may not read the project is answered the same 404 an
	// unknown pid gets).
	TargetTypeAsset: `
SELECT CASE
         WHEN EXISTS (SELECT 1 FROM project_memberships m
                      WHERE m.project_id = a.origin_project_id AND m.user_id = $2::uuid) THEN 'member'
         WHEN p.visibility = 'public' THEN 'public'
         ELSE 'none'
       END
FROM research_assets a
JOIN projects p ON p.id = a.origin_project_id
WHERE a.pid = $1`,

	// A person: everyone may see a live profile, and only the person
	// themselves is a "member" of their own identity — a private event
	// attributed to them (a credit dispute, say) is not for their
	// followers, which is what the member level buys.
	TargetTypeUser: `
SELECT CASE
         WHEN u.id = $2::uuid THEN 'member'
         WHEN u.disabled_at IS NULL THEN 'public'
         ELSE 'none'
       END
FROM users u
WHERE u.id = $1::uuid`,

	// An organization: an active membership, else public unless the
	// organization is deactivated. The membership window is the affiliation
	// date convention — domain.AffiliationWindowSQL, both ends covered —
	// compared date-to-date against the day the caller passes as $3, so a
	// membership that ends today is a membership today (the end date is the
	// last day of the affiliation, not the first day without one).
	//
	// $3 is computed in Go, in UTC (domain.AffiliationDayText), and it is a
	// date, not an instant: the previous form of this predicate converted the
	// date column to timestamptz and compared it with now(), which made the
	// DATABASE SESSION's timezone the arbiter. The same row read 'member'
	// through a UTC-12 session and 'public' through a UTC one. A date column
	// has one meaning in every session.
	//
	// Note that deactivated_at above is a timestamptz and is compared with
	// now() on purpose: that column names an instant, so it has no day to be
	// read in.
	TargetTypeOrganization: `
SELECT CASE
         WHEN EXISTS (SELECT 1 FROM organization_memberships om
                      WHERE om.organization_id = o.id AND om.user_id = $2::uuid
                        AND ` + domain.AffiliationWindowSQL("om", "$3::date") + `) THEN 'member'
         WHEN o.deactivated_at IS NULL OR o.deactivated_at > now() THEN 'public'
         ELSE 'none'
       END
FROM organizations o
WHERE o.id = $1::uuid`,
}

// knowledgeAudience resolves one user's level on a published knowledge
// object, addressed by its PID.
//
// # Why it is not a row of audienceQueries
//
// The other four types resolve to a level inside SQL, because their rule is
// a property of the target's row. A publication's is not: whether the
// network may read it is knowledgepublish.AudienceFor — three axes (the
// version's own visibility_policy_id, the owning project's visibility, the
// rights document's metadata axis) whose documented meaning is "发布不等于公开":
// a version pinned to a policy of its own, or a rights document that
// declares a metadata visibility, stays members-only even in a public
// project. Restating those three axes as a CASE here would be a second
// implementation of the rule the publish path and the read path already
// share — and the one place a divergence would show up is a publication
// being delivered to subscribers its read path refuses. So the query reads
// the three axes as DATA and the rule is asked in Go.
//
// # The levels
//
//   - a member of the owning project: 'member'. Membership is the
//     relationship to the target — the same relationship the asset query
//     resolves, and the level at which private events are delivered.
//   - anyone else: 'public' when AudienceFor answers AudienceNetwork (the
//     publication is readable by an anonymous caller, so a follower who is
//     not in the project may be told it happened), 'none' otherwise.
//
// Fail-closed, as everywhere in this file: an unknown pid reads no row and
// is AudienceNone; a row whose rights document does not parse states no
// metadata axis and can never reach AudienceNetwork; a publication that is
// members-only answers 'none' to a non-member rather than 'public'.
func knowledgeAudience(ctx context.Context, db DBTX, target Target, userID string) (AudienceLevel, error) {
	var (
		member     bool
		projectVis string
		policyID   *string
		rightsJSON []byte
	)
	err := db.QueryRow(ctx, knowledgeAudienceQuery, target.ID, userID).Scan(&member, &projectVis, &policyID, &rightsJSON)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No publication has that pid: no relationship, no delivery.
			return AudienceNone, nil
		}
		return AudienceNone, fmt.Errorf("events: resolve audience for %s %s: %w", target.Type, target.ID, err)
	}
	if member {
		return AudienceMember, nil
	}
	doc, err := rights.Parse(rightsJSON)
	if err != nil {
		// An unreadable rights document resolves no metadata axis, so it can
		// never be a network audience. AudienceFor would refuse it too (the
		// zero document's metadata axis is not project_policy); the branch is
		// explicit so that stays true if rights.Parse ever becomes lenient.
		return AudienceNone, nil
	}
	if knowledgepublish.AudienceFor(projectVis, policyID, doc) == knowledgepublish.AudienceNetwork {
		return AudiencePublic, nil
	}
	return AudienceNone, nil
}

// knowledgeAudienceQuery reads the three inputs AudienceFor decides with,
// plus whether the caller is a member of the project that owns the object.
// It reads an unparseable rights document rather than filtering the row
// out: the audience rule is what refuses it, and a row that vanished from
// the query would be refused by nothing.
//
// The publications are addressed by pid (00083/00090): kp.id never leaves
// the process.
const knowledgeAudienceQuery = `
SELECT EXISTS (SELECT 1 FROM project_memberships m
               WHERE m.project_id = so.project_id AND m.user_id = $2::uuid),
       p.visibility,
       sov.visibility_policy_id::text,
       kp.rights_json
FROM knowledge_publications kp
JOIN scientific_object_versions sov ON sov.id = kp.object_version_id
JOIN scientific_objects so ON so.id = sov.object_id
JOIN projects p ON p.id = so.project_id
WHERE kp.pid = $1`

// TargetAudienceFor is TargetAudience on the store's own pool, for callers
// that are not already inside a transaction (the application service's
// subscribe-time check). It is the same query, deliberately not a copy:
// the rule has one implementation.
func (s *SubscriptionStore) TargetAudienceFor(ctx context.Context, target Target, userID string) (AudienceLevel, error) {
	return s.TargetAudience(ctx, s.pool, target, userID)
}

// ProjectOrganization resolves the organization that owns a project, for
// the fan-out's organization target (eventTargets has no organization id
// to read: no producer writes one into a payload). An unowned project
// (projects.organization_id is nullable) or an unknown project answers "".
func (s *SubscriptionStore) ProjectOrganization(ctx context.Context, db DBTX, projectID string) (string, error) {
	var org *string
	err := db.QueryRow(ctx,
		`SELECT organization_id::text FROM projects WHERE id = $1::uuid`, projectID).Scan(&org)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("events: resolve the organization of project %s: %w", projectID, err)
	}
	if org == nil {
		return "", nil
	}
	return *org, nil
}

// LiveSubscriptionsForTarget returns every live subscription pointing at
// one target — the fan-out's candidate set. Neither decision that follows
// is made here: the caller resolves each candidate's level through
// TargetAudience (one rule, one place) and matches the event type through
// SubscribesTo. Event filters deliberately do NOT narrow this query: a
// subscription whose filters skip this event type must still be
// re-evaluated for revocation, because whether its owner may see the
// target any more has nothing to do with which event types it wanted.
func (s *SubscriptionStore) LiveSubscriptionsForTarget(ctx context.Context, db DBTX, target Target) ([]Subscription, error) {
	rows, err := db.Query(ctx, `
		SELECT `+subscriptionCols+`
		FROM subscriptions
		WHERE target_type = $1 AND target_id = $2 AND deleted_at IS NULL
		ORDER BY created_at, id`, target.Type, target.ID)
	if err != nil {
		return nil, fmt.Errorf("events: claim subscriptions for %s %s: %w", target.Type, target.ID, err)
	}
	defer rows.Close()
	out := []Subscription{}
	for rows.Next() {
		sub, err := scanSubscription(rows)
		if err != nil {
			return nil, fmt.Errorf("events: claim subscriptions: scan: %w", err)
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// InsertDeliveries fans one event out to one subscription's channels. The
// web channel is born delivered — the inbox row IS the delivery, and
// read/unread on top of it is T1003's — while email is born pending for
// the digest sender (T1005). The insert is idempotent per (subscription,
// event, channel), so a retried fan-out is a no-op rather than a
// duplicate. It returns how many rows it actually wrote.
func (s *SubscriptionStore) InsertDeliveries(ctx context.Context, db DBTX, sub Subscription, eventID, eventType string) (int, error) {
	if len(sub.Channels) == 0 {
		return 0, nil
	}
	tag, err := db.Exec(ctx, `
		INSERT INTO subscription_deliveries
		    (subscription_id, user_id, event_id, channel, event_type, target_type, target_id, status, delivered_at)
		SELECT $1, $2, $3, ch, $4, $5, $6,
		       CASE WHEN ch = 'web' THEN 'delivered' ELSE 'pending' END,
		       CASE WHEN ch = 'web' THEN now() ELSE NULL END
		FROM unnest($7::text[]) AS ch
		ON CONFLICT (subscription_id, event_id, channel) DO NOTHING`,
		sub.ID, sub.UserID, eventID, eventType, sub.TargetType, sub.TargetID, sub.Channels)
	if err != nil {
		return 0, fmt.Errorf("events: insert deliveries for subscription %s: %w", sub.ID, err)
	}
	return int(tag.RowsAffected()), nil
}

// Deliveries returns one user's delivery rows, newest first, for the
// pipeline's own tests and the inbox read. status filters when non-empty
// (DeliveryPending, DeliveryDelivered, DeliveryCancelled).
func (s *SubscriptionStore) Deliveries(ctx context.Context, db DBTX, userID, status string, limit int) ([]SubscriptionDelivery, error) {
	rows, err := db.Query(ctx, `
		SELECT `+deliveryCols+`
		FROM subscription_deliveries
		WHERE user_id = $1 AND ($2 = '' OR status = $2)
		ORDER BY created_at DESC, id DESC
		LIMIT $3`, userID, status, limit)
	if err != nil {
		return nil, fmt.Errorf("events: list deliveries: %w", err)
	}
	defer rows.Close()
	out := []SubscriptionDelivery{}
	for rows.Next() {
		d, err := scanSubscriptionDelivery(rows)
		if err != nil {
			return nil, fmt.Errorf("events: list deliveries: scan: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// rowScanner is the shared shape of pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanSubscription(row rowScanner) (Subscription, error) {
	var (
		sub Subscription
		del *time.Time
	)
	if err := row.Scan(&sub.ID, &sub.UserID, &sub.TargetType, &sub.TargetID,
		&sub.EventFilters, &sub.Channels, &sub.CreatedAt, &sub.UpdatedAt, &del); err != nil {
		return Subscription{}, err
	}
	sub.DeletedAt = del
	return sub, nil
}

func scanSubscriptionDelivery(row rowScanner) (SubscriptionDelivery, error) {
	var d SubscriptionDelivery
	if err := row.Scan(&d.ID, &d.SubscriptionID, &d.UserID, &d.EventID, &d.Channel,
		&d.EventType, &d.TargetType, &d.TargetID, &d.Status, &d.CreatedAt,
		&d.DeliveredAt, &d.CancelledAt, &d.ReadAt); err != nil {
		return SubscriptionDelivery{}, err
	}
	return d, nil
}

// withSubscriptionNotFound translates the absent-row case into the
// package sentinel (both scans above answer pgx.ErrNoRows when the id is
// unknown or not the caller's).
func withSubscriptionNotFound(sub Subscription, err error) (Subscription, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return Subscription{}, ErrSubscriptionNotFound
	}
	if err != nil {
		return Subscription{}, fmt.Errorf("events: read subscription: %w", err)
	}
	return sub, nil
}

// emptyIfNil renders a nil slice as an empty array: the column defaults
// only apply to omitted columns, and pgx renders nil as NULL — a NULL
// channels column would fail the NOT NULL CHECK with a confusing error.
func emptyIfNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// isUniqueViolation reports whether err is a unique-index violation on
// the named index. The name is checked so an unrelated uniqueness rule
// (a future one) is not silently reported as "already subscribed".
func isUniqueViolation(err error, index string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23505" && pgErr.ConstraintName == index
}
