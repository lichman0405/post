package notifications

import (
	"fmt"
	"html"
	"net/url"
	"strings"
	"time"

	"github.com/lichman0405/post/internal/events"
)

// The digest template (docs/18 §4: "Email immediate/digest abstraction";
// this task's requirement: templates concise). Two decisions, both made
// here rather than in the sender:
//
//   - WHAT one item says: when it happened, the event type, the target's
//     own label, and a link to the target's page. Nothing else — no event
//     payload, no ids beyond the target's own link. The delivery row is a
//     pointer and this email stays a pointer too: an email is a copy that
//     has left the building (it sits in mailboxes, forwards and backups
//     nobody can reach), so the less of the research record it carries, the
//     less there is to leak later. The subject's LABEL is the one piece of
//     content, and it is only ever read behind the audience gate (see
//     events.NotificationStore.TargetLabel).
//
//   - HOW it is laid out: one line per event, a link on its own line, and
//     one footer line naming the frequency and where to change it. No
//     images, no tracking, no markup beyond a list and the links — the
//     GitHub/Primer restraint of docs/10 §51 applied to the one surface
//     that cannot be restyled later.
//
// The item's ORDER is the sender's (oldest first): a template that sorted
// would be a second opinion about the digest.

// Item is one notification in a digest.
type Item struct {
	EventType string
	Target    events.Target
	// Label is the target's own label (a project's name, an asset's title,
	// …), read behind the audience gate. Empty when the subject has no
	// readable label; the renderer then names the target by its identity.
	Label string
	// OccurredAt is when the event happened (research_events.occurred_at),
	// not when the digest was built.
	OccurredAt time.Time
	// Link is the absolute URL of the target's page, or "" when the
	// surface has no page yet.
	Link string
}

// Digest is everything one message says.
type Digest struct {
	To    string
	Items []Item
	// ManageURL is where the recipient changes their cadence (the
	// notifications page). Empty is rendered as "in your POST notification
	// settings" rather than as a dangling link.
	ManageURL string
}

// TargetPath is the web path of a target's own page, or "" when the surface
// has no page yet. A digest links to what EXISTS: the project, asset and
// person pages are served today (apps/web/app/(main)/projects/[id],
// /assets/[pid], /users/[id]); the knowledge object and organization
// surfaces are specified (specs/ui/page-inventory.csv lines 15 and 17 —
// /knowledge/{id}, /orgs/{org}) but not served, so they are left unlinked
// rather than pointed at a route that would 404.
func TargetPath(t events.Target) string {
	switch t.Type {
	case events.TargetTypeProject:
		return "/projects/" + url.PathEscape(t.ID)
	case events.TargetTypeAsset:
		return "/assets/" + url.PathEscape(t.ID)
	case events.TargetTypeUser:
		return "/users/" + url.PathEscape(t.ID)
	default:
		return ""
	}
}

// Render renders d as a Mail. Both bodies come from the same items, so the
// two parts of a multipart message can never disagree about what was sent.
func Render(d Digest) Mail {
	return Mail{
		To:      d.To,
		Subject: subject(d),
		Text:    renderText(d),
		HTML:    renderHTML(d),
	}
}

// subject is deliberately content-free beyond the count: a subject line is
// the one part of a message that is shown in a list of messages, on lock
// screens and in notifications, so it carries no research content at all.
func subject(d Digest) string {
	n := len(d.Items)
	if n == 1 {
		return "POST digest: 1 update"
	}
	return fmt.Sprintf("POST digest: %d updates", n)
}

func renderText(d Digest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "POST research digest — %s\n\n", count(d))
	for _, it := range d.Items {
		fmt.Fprintf(&b, "- %s %s — %s\n",
			it.OccurredAt.UTC().Format(time.RFC3339), it.EventType, describe(it))
		if it.Link != "" {
			fmt.Fprintf(&b, "  %s\n", it.Link)
		}
	}
	b.WriteString("\n")
	b.WriteString(footer(d))
	b.WriteString("\n")
	return b.String()
}

func renderHTML(d Digest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<p>POST research digest — %s</p>\n<ul>\n", html.EscapeString(count(d)))
	for _, it := range d.Items {
		fmt.Fprintf(&b, "  <li>%s <code>%s</code> — %s",
			html.EscapeString(it.OccurredAt.UTC().Format(time.RFC3339)),
			html.EscapeString(it.EventType),
			html.EscapeString(describe(it)))
		if it.Link != "" {
			// The attribute is HTML-escaped, not Go-quoted: a backslash is
			// not an HTML escape, so %q would leave the quote that ends the
			// attribute and hand the rest of the value to the client as
			// markup (see TestRenderHTMLEscapesTheLink).
			fmt.Fprintf(&b, "<br><a href=\"%s\">%s</a>", html.EscapeString(it.Link), html.EscapeString(it.Link))
		}
		b.WriteString("</li>\n")
	}
	b.WriteString("</ul>\n")
	fmt.Fprintf(&b, "<p>%s</p>\n", renderFooterHTML(d))
	return b.String()
}

// footer is the one line every digest ends with: where to change what
// reaches you. The URL is optional — a digest built without one still says
// what to do, rather than printing a dangling link.
func footer(d Digest) string {
	if d.ManageURL == "" {
		return "Manage what reaches you in your POST notification settings."
	}
	return "Manage what reaches you: " + d.ManageURL
}

func renderFooterHTML(d Digest) string {
	if d.ManageURL == "" {
		return "Manage what reaches you in your POST notification settings."
	}
	return "Manage what reaches you: " +
		"<a href=\"" + html.EscapeString(d.ManageURL) + "\">" + html.EscapeString(d.ManageURL) + "</a>."
}

// count renders "1 update" / "N updates".
func count(d Digest) string {
	if len(d.Items) == 1 {
		return "1 update"
	}
	return fmt.Sprintf("%d updates", len(d.Items))
}

// describe names one item's subject: the target's label when it has one,
// its identity otherwise (an asset's pid reads as well as a label; a project
// uuid is worse than a label but better than a blank line).
func describe(it Item) string {
	label := it.Label
	if label == "" {
		label = it.Target.Type + " " + it.Target.ID
	}
	return fmt.Sprintf("%s (%s)", label, it.Target.Type)
}
