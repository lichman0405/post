package notifications

import (
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/events"
)

// T1005 unit suite for the digest template. Two properties are what this
// file is for:
//
//   - the message stays CONCISE (the task's requirement): one line per
//     event, no payload, no imagery, no tracking — pinned by asserting the
//     message contains nothing beyond what it is supposed to;
//   - the message carries only what the gate allowed: the subject line is
//     content-free, and the item names its target through the label the
//     caller resolved AFTER the audience check.

func testItem(target events.Target, label string) Item {
	return Item{
		EventType:  "state.committed",
		Target:     target,
		Label:      label,
		OccurredAt: time.Date(2026, 9, 16, 7, 5, 0, 0, time.UTC),
		Link:       TargetPath(target),
	}
}

func TestRenderSubjectIsContentFree(t *testing.T) {
	// The subject is the part that shows on a lock screen and in a message
	// list, so it says how many and nothing else — no target name, no event
	// type, no id.
	one := Render(Digest{To: "a@example.com", Items: []Item{
		testItem(events.Target{Type: events.TargetTypeProject, ID: "p-1"}, "Project Zephyr"),
	}})
	if one.Subject != "POST digest: 1 update" {
		t.Errorf("subject = %q, want the count-only form", one.Subject)
	}
	many := Render(Digest{To: "a@example.com", Items: []Item{
		testItem(events.Target{Type: events.TargetTypeProject, ID: "p-1"}, "Project Zephyr"),
		testItem(events.Target{Type: events.TargetTypeProject, ID: "p-2"}, "Project Aurora"),
	}})
	if many.Subject != "POST digest: 2 updates" {
		t.Errorf("subject = %q, want the plural count-only form", many.Subject)
	}
	for _, m := range []Mail{one, many} {
		for _, secret := range []string{"Zephyr", "Aurora", "p-1", "p-2", "state.committed"} {
			if strings.Contains(m.Subject, secret) {
				t.Errorf("the subject %q carries %q: the subject must name no target and no event", m.Subject, secret)
			}
		}
	}
}

func TestRenderTextIsConciseAndComplete(t *testing.T) {
	target := events.Target{Type: events.TargetTypeProject, ID: "0b6f5b1e-6f4a-4a1f-9d3e-2c7a5b8e4d10"}
	item := testItem(target, "Project Zephyr")
	// The absolute URL is the SENDER's (it owns the configured origin); the
	// template renders the link it is given.
	item.Link = "http://web.test" + TargetPath(target)
	mail := Render(Digest{
		To:        "subscriber@example.com",
		Items:     []Item{item},
		ManageURL: "http://web.test/notifications",
	})

	// Complete: when it happened, what happened, where, and how to leave.
	for _, want := range []string{
		"2026-09-16T07:05:00Z",
		"state.committed",
		"Project Zephyr",
		"http://web.test/projects/" + target.ID,
		"http://web.test/notifications",
	} {
		if !strings.Contains(mail.Text, want) {
			t.Errorf("the text body is missing %q:\n%s", want, mail.Text)
		}
	}
	// Concise: nothing that is not one of those four things. A body that
	// grew a payload dump, a signature block or an unsubscribe landing page
	// would still pass the checks above — this is the half that fails.
	for _, unwanted := range []string{"<html", "<div", "<table", "<img", "utm_", "payload", "{"} {
		if strings.Contains(strings.ToLower(mail.Text), unwanted) {
			t.Errorf("the text body carries %q:\n%s", unwanted, mail.Text)
		}
	}
	if n := strings.Count(mail.Text, "\n"); n > 8 {
		t.Errorf("the text body is %d lines for one item, want a handful (concise):\n%s", n, mail.Text)
	}
}

func TestRenderHTMLEscapesTheLabel(t *testing.T) {
	// A label is a project's name, which the product lets a user type. It is
	// HTML-escaped rather than trusted: an unescaped one would be a stored
	// XSS in every subscriber's mail client that renders HTML.
	label := `<img src=x onerror="alert(1)"> & "Project"`
	mail := Render(Digest{
		To:    "subscriber@example.com",
		Items: []Item{testItem(events.Target{Type: events.TargetTypeProject, ID: "p-1"}, label)},
	})
	if strings.Contains(mail.HTML, "<img") {
		t.Errorf("the HTML body carries the raw label as markup:\n%s", mail.HTML)
	}
	if !strings.Contains(mail.HTML, "&lt;img") || !strings.Contains(mail.HTML, "&amp;") {
		t.Errorf("the label is not escaped in the HTML body:\n%s", mail.HTML)
	}
	// The text body is not HTML: the label is verbatim there (escaping it
	// would show a reader entity soup).
	if !strings.Contains(mail.Text, label) {
		t.Errorf("the text body should carry the label verbatim:\n%s", mail.Text)
	}
}

func TestRenderHTMLEscapesTheLink(t *testing.T) {
	// The link goes into an ATTRIBUTE, which is a different parsing context
	// from the text it is also printed as: a quote there ends the attribute
	// and starts a new one. The label above is already escaped; this is the
	// same rule for the URL, and it must not depend on the caller having
	// escaped anything first — the template is the boundary that leaves the
	// building.
	// The injected value, and the attribute it must stay inside. A backslash
	// is NOT an HTML escape: `href="a\"` still ends the attribute at the
	// quote, so a value rendered that way hands `onmouseover` to the client
	// as a real attribute. The escaped form is what the value must carry.
	const injected = `" onmouseover="alert(1)`
	const escaped = "&#34; onmouseover=&#34;alert(1)"
	item := testItem(events.Target{Type: events.TargetTypeProject, ID: "p-1"}, "Project Zephyr")
	item.Link = "http://web.test/projects/p-1" + injected
	// The item link and the footer link are rendered by two different code
	// paths, so each case carries its own URL.
	for _, tc := range []struct {
		name string
		d    Digest
		want string
	}{
		{
			name: "the item link",
			d:    Digest{To: "subscriber@example.com", Items: []Item{item}},
			want: `href="http://web.test/projects/p-1` + escaped + `"`,
		},
		{
			name: "the footer link",
			d:    Digest{To: "subscriber@example.com", ManageURL: "http://web.test/notifications" + injected},
			want: `href="http://web.test/notifications` + escaped + `"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mail := Render(tc.d)
			if !strings.Contains(mail.HTML, tc.want) {
				t.Errorf("the rendered link is not the escaped attribute %q — the value did not stay inside "+
					"its attribute:\n%s", tc.want, mail.HTML)
			}
			if strings.Contains(mail.HTML, `\"`) {
				t.Errorf("the HTML body carries a backslash-escaped quote (a Go string literal, not HTML):\n%s",
					mail.HTML)
			}
		})
	}
}

func TestRenderFooterWithoutAManageURL(t *testing.T) {
	// With no configured origin there is no URL to point at, and a dangling
	// link is worse than none: the footer still says what to do. (The item
	// link is empty too — this is the digest a worker with no
	// POST_WEB_ORIGIN and no served page would render.)
	item := testItem(events.Target{Type: events.TargetTypeProject, ID: "p-1"}, "Project Zephyr")
	item.Link = ""
	mail := Render(Digest{To: "subscriber@example.com", Items: []Item{item}})
	if strings.Contains(mail.Text, "http") || strings.Contains(mail.HTML, "href") {
		t.Errorf("a digest with no manage URL renders a link anyway:\n%s\n%s", mail.Text, mail.HTML)
	}
	if !strings.Contains(mail.Text, "Manage what reaches you") {
		t.Errorf("the footer is missing:\n%s", mail.Text)
	}
}

func TestRenderFallsBackToTheTargetIdentity(t *testing.T) {
	// The label is read behind the gate and may legitimately be empty (the
	// subject has no readable name). A blank line would tell the reader
	// nothing, so the identity is used — worse than a label, better than
	// nothing.
	pid := "01j9z6k3m4n5p6q7r8s9t0v1a1"
	mail := Render(Digest{
		To:    "subscriber@example.com",
		Items: []Item{testItem(events.Target{Type: events.TargetTypeAsset, ID: pid}, "")},
	})
	if !strings.Contains(mail.Text, "asset "+pid) {
		t.Errorf("an empty label did not fall back to the target identity:\n%s", mail.Text)
	}
}

func TestTargetPath(t *testing.T) {
	cases := []struct {
		target events.Target
		want   string
	}{
		{events.Target{Type: events.TargetTypeProject, ID: "abc"}, "/projects/abc"},
		{events.Target{Type: events.TargetTypeAsset, ID: "01j9z6k3"}, "/assets/01j9z6k3"},
		{events.Target{Type: events.TargetTypeUser, ID: "u-1"}, "/users/u-1"},
		// No page is served for these two, so there is no path: pointing at
		// a route that 404s would be a broken link in every digest. (The
		// pages exist for project/asset/user — apps/web/app/(main)/….)
		{events.Target{Type: events.TargetTypeKnowledge, ID: "k-1"}, ""},
		{events.Target{Type: events.TargetTypeOrganization, ID: "o-1"}, ""},
		{events.Target{Type: "planet", ID: "p-1"}, ""},
	}
	for _, tc := range cases {
		if got := TargetPath(tc.target); got != tc.want {
			t.Errorf("TargetPath(%s %s) = %q, want %q", tc.target.Type, tc.target.ID, got, tc.want)
		}
	}
	// A target id is data from a table; a path segment that escaped it would
	// resolve to a DIFFERENT route (a slash, a dot-dot).
	if got := TargetPath(events.Target{Type: events.TargetTypeProject, ID: "../../etc/passwd"}); strings.Contains(got, "../") {
		t.Errorf("TargetPath = %q, want the id escaped", got)
	}
}

func TestRenderBodiesAgree(t *testing.T) {
	// The two bodies are rendered from the same items; a digest whose HTML
	// listed something the text did not (or the other way round) would be
	// two different messages with one claim to what was sent.
	items := []Item{
		testItem(events.Target{Type: events.TargetTypeProject, ID: "p-1"}, "Project Zephyr"),
		testItem(events.Target{Type: events.TargetTypeAsset, ID: "a-1"}, "Dataset One"),
	}
	mail := Render(Digest{To: "a@example.com", Items: items, ManageURL: "http://web.test/notifications"})
	if t1, t2 := strings.Count(mail.Text, "- 20"), strings.Count(mail.HTML, "<li>"); t1 != len(items) || t2 != len(items) {
		t.Errorf("text items = %d, html items = %d, want %d each", t1, t2, len(items))
	}
	for _, it := range items {
		if !strings.Contains(mail.Text, it.Label) || !strings.Contains(mail.HTML, it.Label) {
			t.Errorf("item %q is missing from one of the two bodies", it.Label)
		}
	}
}
