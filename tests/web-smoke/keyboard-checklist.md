# Manual keyboard smoke checklist (T1104)

This document is the human half of the keyboard accessibility gate for
WCAG 2.2 AA (`docs/28_ACCESSIBILITY_I18N.md`). The automatic half lives in
`a11y-smoke.mjs`. **The two are not interchangeable.** The automatic walk
asserts a specific, bounded list of behaviours in headless Chromium; it
cannot be used to claim anything outside that list, and this document says
which is which.

## Running the app

From the repository root:

```bash
make a11y        # real PostgreSQL + real API handlers + built web app + Chromium
```

The suite prints the fixture routes it scanned (the project, PR, release,
asset and user URLs). Copy those — the ids are generated per run, so the
`<uuid>` placeholders below have to be replaced with the ones this run
printed.

`make a11y` builds and tears the whole stack down again when it finishes. To
walk the pages by hand, run those two servers yourself and leave them up.
These are the same commands `run-a11y.sh` runs (`tests/web-smoke/run-a11y.sh`
builds and starts the harness at :86-112 and the web app at :114-125; the
ports are its defaults at :23-24):

```bash
# 1) the API harness — real PostgreSQL, real API handlers, seeded fixtures.
#    It prints ONE `READY {json}` line with every fixture id
#    (project_id, pr_number, release_id, asset_pid/version, user_id, …):
#    that line is the URL list. Leave it running.
cd "$(git rev-parse --show-toplevel)"
export POSTGRES_TEST_ADMIN_URL=postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post
#    The port has to be stated here. The harness's own fallback is
#    127.0.0.1:18192 (tests/web-smoke/a11y-harness/main.go:98); the web app in
#    step 2 is built against 18193, which is what the suite picks
#    (run-a11y.sh:23, exported to the harness at :29). Leave this line out and
#    the two halves listen on different ports: HTTP still returns 200 and every
#    dynamic page renders its not-found shell, because nothing is answering on
#    the origin the browser bundle was built with.
export A11Y_API_ADDR=127.0.0.1:18193
go build -o /tmp/a11y-harness ./tests/web-smoke/a11y-harness
/tmp/a11y-harness

# 2) the web app, in a second terminal, built against that harness.
#    Leave it running too.
cd apps/web
POST_ENV=prod API_BASE_URL=http://127.0.0.1:18193 \
  SCIENTIFIC_ADAPTER_URL=http://127.0.0.1:19100 pnpm run build
POST_ENV=prod API_BASE_URL=http://127.0.0.1:18193 \
  SCIENTIFIC_ADAPTER_URL=http://127.0.0.1:19100 \
  ./node_modules/.bin/next start -p 31108

# 3) the third "process" is the browser you walk it in:
#    http://127.0.0.1:31108 — a real one, mouse untouched.
```

If 31108 or 18193 is taken, change the number in **both** steps — the
`export` in step 1, and `API_BASE_URL` (twice: build and start) plus
`next start -p` in step 2. There is no automatic fallback, and a mismatch is
silent: the harness defaults to 18192 and `next start` binds whatever `-p`
says, so a half-changed pair still starts and still serves pages.

Do not try to borrow a `make a11y` run's ids instead. That target tears the
stack down as it exits — its EXIT trap kills both servers
(`run-a11y.sh:50-62`), and the harness drops its own fixture database on the
way out (`a11y-harness/main.go:114` → `DROP DATABASE … WITH (FORCE)` at
`:660`) — so the `READY` ids name a process and a database that are both gone
by the time the target returns. The ids in the document below come from a
walkthrough run exactly as printed above; see "Walkthrough verified" at the
end.

Use a real browser, and do the whole document without touching the mouse.

---

## What the automatic walk ALREADY proves

Every line below is asserted by `a11y-smoke.mjs` in headless Chromium, and
the run fails if any of them stops being true. Do not re-check these by hand
as if they were unverified — but do read them, because they are the set of
things you may NOT claim the manual pass discovered.

On `/` (desktop, 1280×800):

- the skip link is the first Tab stop and is visibly exposed on focus;
- `Enter` on the skip link moves focus to `<main id="main">`;
- Tab walks the global header in DOM order — Skip to content, POST home,
  Search POST, Home, Explore, Projects, Assets, People, Organizations,
  Notifications, Sign in — with a visible focus indicator on every stop;
- the search form submits to `/search?q=catalyst` by keyboard alone, and the
  page shows the submitted query;
- on that same route the answer itself really renders (`[data-search-answer]`
  is in the DOM, and the error state is not) — so what section B3 below asks
  you to listen for is a live region that is actually there to announce;
- `Enter` on the Projects nav link navigates to `/projects` and the heading
  there is "Projects";
- no element on the page has a positive `tabindex`.

On `/` (375×667):

- the mobile menu button is reachable by Tab, opens with `Enter`, moves focus
  to the first drawer item, walks one item with Tab, and closes with `Escape`
  returning focus to the toggle.

On the **pull request** route (only when the suite is run with fixture data,
i.e. `make a11y`):

- the tablist is a single tab stop (roving tabindex: exactly one tab has
  `tabIndex=0`);
- `ArrowRight` / `ArrowLeft` move between tabs, `End` reaches the last tab,
  `ArrowRight` wraps from the last to the first, and the focused tab is the
  one with `aria-selected="true"`.

On `/` (any route, both media states):

- a probe element carrying a 2 s animation really does run at 2 s with no
  motion preference, and really is collapsed to ≈0 s with
  `prefers-reduced-motion: reduce` — i.e. the guard in `globals.css` is
  applied by the browser, not merely present in the stylesheet.

**What the automatic walk does NOT prove**, and therefore what the manual
sections below exist for:

- anything requiring a **screen reader**. No audio is inspected anywhere:
  that a live region announces, that an accessible name is *spoken*
  usefully, and that focus order is *perceived* are all outside what
  Chromium can be asked. Every live region T1104 added is reasoned about,
  not heard: the load states (`auth-status.tsx`, `project-shell.tsx`, the
  three loading cards, `search-answer.tsx`) and the submit results
  (`login-card.tsx` failure Flash, `pulls/[number]/page.tsx` "Recorded …" /
  review error, `profile-card.tsx` "Profile updated." / save error,
  `settings/page.tsx`, `releases/page.tsx` and `milestones/page.tsx` notices,
  `conflicts/page.tsx` saved chip / save error, `inbox-surface.tsx` mark-read
  notice). What *is* checked automatically is narrower than the claim they
  carry: that each exists in the DOM with the role it needs after the real
  write (see "Walkthrough verified" at the end).
- any route other than `/` and the pull request route;
- the project pages, the auth surface, and every form other than search;
- zoom/reflow, contrast ratios against a real display, and OS-level
  high-contrast or reduced-motion settings.

---

## Section A — Global header and skip link (on `/`)

Starting URL: `/`

| Step | Keys | Expected |
|------|------|----------|
| A1 | `Tab` | Focus ring on "Skip to content" (top-left). |
| A2 | `Enter` | Focus moves into `<main id="main">`; the next `Tab` lands on the first focusable control inside main. |
| A3 | `Shift+Tab` | Focus returns to the skip link and it is still visible. |
| A4 | `Tab` repeatedly | Every header control in DOM order: POST home → Search POST → Home → Explore → Projects → Assets → People → Organizations → Notifications → Sign in. No invisible focus, no jumps. |

A1, A2 and A4 are also automated; doing them by hand confirms the browser
you are using agrees with headless Chromium. A3 is manual only.

## Section B — Search (on `/`)

| Step | Keys | Expected |
|------|------|----------|
| B1 | `Tab` to the search input | Focus ring on the "Search POST" input. |
| B2 | Type `catalyst`, `Enter` | Navigates to `/search?q=catalyst`; the page shows the query. |
| B3 | With the answer rendered, wait for it | The loading line is replaced by an answer or by a named failure — **never a blank panel**. Check with a screen reader on that the change is announced; the `role="status"` / `role="alert"` regions are what should carry it. |

B1 and B2 are automated. B3 exists only for the screen-reader half.

## Section C — Auth layout skip link (on `/login`)

Starting URL: `/login`

T1104 added the skip link to the `(auth)` layout, mirroring the one in
`(main)`. This section is **manual only** — no automated check covers
`/login`'s keyboard path.

| Step | Keys | Expected |
|------|------|----------|
| C1 | `Tab` once | Focus ring on "Skip to content". |
| C2 | `Enter` | Focus moves to `<main id="main">`; the sign-in card is inside it. |
| C3 | `Tab` through the card | Email, password, submit — each shows a visible focus indicator. |
| C4 | `Shift+Tab` from the first card control | Focus walks back out through the skip link, not into a trap. |

## Section D — Project shell and tabs (on `/projects/<uuid>`)

Starting URL: the project overview URL `make a11y` printed.

| Step | Keys | Expected |
|------|------|----------|
| D1 | `Tab` | First stop is the global skip link. |
| D2 | `Tab` through the header | Same order as A4; the project tabs come after the header. |
| D3 | `Enter` on a project tab | The tab is marked current and the main column changes. |
| D4 | `Tab` into the main column | Focus moves through project content; links and buttons show focus rings. |
| D5 | Read the "Primary question" panel | If the project has no accepted question the panel says so; it is never an empty heading. |

## Section E — Research page (on `/projects/<uuid>/research`)

| Step | Keys | Expected |
|------|------|----------|
| E1 | `Tab` to the research outline | The question/finding entries are reachable and show focus. |
| E2 | `Enter` on an entry | Navigates to the selected branch or object. |
| E3 | `Tab` through the page | No keyboard trap: focus reaches every interactive element and leaves the page again. |

This is the surface `docs/42` calls out for a structured outline/list
fallback in place of the graph, so a keyboard-only reader must be able to
get everywhere a mouse could. Nothing here is automated.

## Section F — Pull request tabs (on `/projects/<uuid>/pulls/1`)

The tablist is automated (see above). The manual pass is here for the
screen-reader reading of it.

| Step | Keys | Expected |
|------|------|----------|
| F1 | `Tab` to the tablist | **One** stop lands on Summary — not six stops, one per tab. |
| F2 | `ArrowRight` / `ArrowLeft` | Moves through Summary → Scientific changes → Knowledge changes → Evidence → Checks → Raw, and the panel below changes with it. |
| F3 | `Home` / `End` | First and last tab. |
| F4 | `Tab` from an arrow-selected tab | Focus moves *into* the panel, not to the next tab. |
| F5 | With a screen reader | The tablist announces itself as tabs, and the selected tab is the one announced as selected. |

F1–F4 are automated; F5 is the reason this section still exists.

## Section G — Motion preference

| Step | Action | Expected |
|------|--------|----------|
| G1 | Set the OS to "reduce motion" (or emulate it in devtools), then load any page with a loading state | Nothing spins or slides. The visible "Loading …" text is still there — freezing motion must not remove the status. |
| G2 | With a screen reader, trigger a load | The loading state is announced once, not twice (the spinner's own label is suppressed in favour of the visible sentence). |

G1's mechanism is automated as a computed-style probe; the OS-level setting
and the announcement are manual.

## Section H — Submit results (the write operations)

Each row is a place where an action the reader takes gets an answer. The
answer must be *reported*, not merely drawn: `role="status"` is polite and
written for success, `role="alert"` is assertive and written for failure.
Nothing is automated here — the DOM side is checked ("Walkthrough verified"),
the hearing of it is not.

| Where | The write | What should be announced |
|-------|-----------|--------------------------|
| `/login` | sign in / sign up / OIDC start fails | the reason, as an alert (`.auth-flash`) |
| `/projects/<uuid>/pulls/<n>` | "Record review" lands | "Recorded <decision>" as a polite status |
| `/projects/<uuid>/pulls/<n>` | "Record review" is rejected | the API's reason, as an alert |
| `/users/<uuid>` | profile save lands / is rejected | "Profile updated." politely; the error alertly |
| `/projects/<uuid>/settings` | "Save settings" | the success or error notice under the form |
| `/projects/<uuid>/releases` | "Create release" | the notice under the form |
| `/projects/<uuid>/milestones` | "Record milestone" | the notice under the form |
| `/projects/<uuid>/conflicts` | "Save decision" | the "resolved" chip appears (polite); a rejection is an alert |
| `/notifications` | "Mark read" / "Mark all read" | the notice above the list (polite) |
| `/search?q=…` | submitting the search | the loading line, then the answer or a *named* failure |

---

## How to fail this checklist

Any of these is a red result:

- a `Tab` press moves focus to something not visibly focused;
- a control activates but focus is lost afterwards (nothing has focus);
- the skip link is missing on `/` or on `/login`;
- Tab order does not match DOM order;
- a positive `tabindex` exists outside a fixture;
- an arrow key does nothing inside a widget that announces itself as a
  tablist, or the tablist costs one Tab stop per tab;
- a loading state is a blank region, or is announced twice, or motion
  continues with reduced motion requested;
- a submit that fails, or succeeds, and the page says nothing (Section H).

## What to record

- browser, OS, and which screen reader (if any) was used;
- whether every section passed, or the first step that failed;
- the routes exercised.

A passing manual run extends the automated one; it does not replace it, and
neither replaces the other's evidence.

---

## Walkthrough verified

The "Running the app" commands above were run exactly as printed (harness on
18193, web app on 31108, nothing else exported), on 2026-09-22 against this
tree, and the paths they describe were walked:

- `READY {…}` printed with `"api_base":"http://127.0.0.1:18193"`, and
  `ss -tln` showed one listener on 18193;
- `GET http://127.0.0.1:18193/api/v1/projects/<project_id>` → `HTTP 200` with
  `"name":"A11Y Test Project"` in the body;
- `GET http://127.0.0.1:31108/projects/<project_id>` → `HTTP 200`, 46,815
  bytes of HTML;
- a real Chromium on that URL: title `A11Y Test Project — POST`, `<h1>` the
  same, the fixture text present, no shell marker;
- a real Chromium on the write paths of Section H: signing in with wrong
  credentials renders `.auth-flash` with `role="alert"`, "Record review"
  renders `[data-review-saved]` with `role="status"` and the text
  `Recorded approved`, and a rejected review renders `[data-review-error]`
  with `role="alert"`.

And the failure the port line prevents was reproduced deliberately: with the
same web build but the harness started **without** `A11Y_API_ADDR`, the
harness reported `"api_base":"http://127.0.0.1:18192"`, nothing listened on
18193, the page still answered `HTTP 200` — and rendered `Project
unavailable` with the fixture text absent. That is why step 1 states the port
rather than relying on the harness's own default.
