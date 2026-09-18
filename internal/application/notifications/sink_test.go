package notifications

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// T1005 unit suite for the development mail sink — the transport the task's
// "dev mail sink" requirement is about. What it must be: a message on disk,
// opening-able, addressed, impossible to steer with message content, and
// INVISIBLE TO GIT (a file of plaintext notification content that `git add
// -A` can reach is content that leaves the building through the history; the
// last test here is the one that pins that).

func testSink(t *testing.T) *DevSink {
	t.Helper()
	sink, err := NewDevSink(t.TempDir(), WithDevSinkLogger(senderTestLogger()),
		WithDevSinkClock(func() time.Time { return time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC) }))
	if err != nil {
		t.Fatalf("NewDevSink: %v", err)
	}
	return sink
}

func TestDevSinkWritesOneMessagePerSend(t *testing.T) {
	sink := testSink(t)
	ctx := context.Background()
	mail := Mail{To: "subscriber@example.com", Subject: "POST digest: 1 update",
		Text: "text body", HTML: "<p>html body</p>"}
	if err := sink.Send(ctx, mail); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := sink.Send(ctx, mail); err != nil {
		t.Fatalf("Send (second): %v", err)
	}

	entries, err := os.ReadDir(sink.dir)
	if err != nil {
		t.Fatalf("read the sink directory: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("the sink holds %d files, want 2 — one file per message (the second send is a retry, "+
			"not an overwrite of the first)", len(entries))
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".eml" {
			t.Errorf("message file %q does not end in .eml", e.Name())
		}
		info, err := e.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", e.Name(), err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %o, want 600: the files carry notification content", e.Name(), perm)
		}
	}

	raw, err := os.ReadFile(filepath.Join(sink.dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read the message: %v", err)
	}
	msg := string(raw)
	for _, want := range []string{
		"To: subscriber@example.com",
		"Subject: POST digest: 1 update",
		"Date: ", "MIME-Version: 1.0",
		"Content-Type: multipart/alternative;",
		"Content-Type: text/plain; charset=utf-8", "\r\ntext body",
		"Content-Type: text/html; charset=utf-8", "<p>html body</p>",
		"\r\n", // the message is CRLF-framed, like the wire format it imitates
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the written message is missing %q:\n%s", want, msg)
		}
	}
	// The two parts share one boundary, and it is the one declared in the
	// Content-Type header: a message whose parts use a different boundary
	// opens as an empty body in every client.
	header := msg[:strings.Index(msg, "\r\n\r\n")]
	boundary := header[strings.Index(header, `boundary="`)+len(`boundary="`):]
	boundary = boundary[:strings.Index(boundary, `"`)]
	if n := strings.Count(msg, "--"+boundary); n != 3 {
		t.Errorf("the boundary %q appears %d times, want 3 (two parts and the closing delimiter)", boundary, n)
	}
}

func TestDevSinkRefusesHeaderInjection(t *testing.T) {
	// A line break in an address or a subject is how a header is forged (a
	// blind-copy recipient, a rewritten Subject). The sink refuses instead
	// of writing the forged message to disk.
	sink := testSink(t)
	cases := []struct {
		name string
		mail Mail
	}{
		{"CRLF in the recipient", Mail{To: "a@example.com\r\nBcc: attacker@example.com", Subject: "s", Text: "t", HTML: "h"}},
		{"bare LF in the recipient", Mail{To: "a@example.com\nBcc: attacker@example.com", Subject: "s", Text: "t", HTML: "h"}},
		{"CRLF in the subject", Mail{To: "a@example.com", Subject: "s\r\nBcc: attacker@example.com", Text: "t", HTML: "h"}},
		{"no recipient", Mail{To: "", Subject: "s", Text: "t", HTML: "h"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := sink.Send(context.Background(), tc.mail); err == nil {
				t.Fatal("Send = nil error, want a refusal")
			}
		})
	}
	entries, err := os.ReadDir(sink.dir)
	if err != nil {
		t.Fatalf("read the sink directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the refused messages left %d files behind, want none", len(entries))
	}
}

func TestDevSinkFileNameIsGenerated(t *testing.T) {
	// The file name must not be derived from the recipient: an address in a
	// path is a traversal surface, and two messages to the same account in
	// the same second must not collide.
	sink := testSink(t)
	ctx := context.Background()
	mail := Mail{To: "../../etc/passwd", Subject: "s", Text: "t", HTML: "h"}
	// (The recipient is legal as a header — it is a nonsense ADDRESS, not a
	// malformed header — so the sink writes it; what matters is where.)
	if err := sink.Send(ctx, mail); err != nil {
		t.Fatalf("Send: %v", err)
	}
	entries, err := os.ReadDir(sink.dir)
	if err != nil {
		t.Fatalf("read the sink directory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("files = %d, want 1", len(entries))
	}
	name := entries[0].Name()
	if strings.Contains(name, "passwd") || strings.Contains(name, "..") {
		t.Errorf("the file name %q is derived from the recipient", name)
	}
	if !strings.HasPrefix(name, "20260916T120000.000000000Z-") {
		t.Errorf("file name %q does not start with the send time", name)
	}
	// Nothing escaped the directory.
	if _, err := os.Stat(filepath.Join(sink.dir, "..", "..", "etc", "passwd")); err == nil {
		t.Error("a file appeared outside the sink directory")
	}
}

func TestNewDevSinkNeedsADirectory(t *testing.T) {
	if _, err := NewDevSink("   "); err == nil {
		t.Error("NewDevSink(\"\") = nil error, want a refusal: a sink that writes messages nowhere " +
			"looks exactly like a working one")
	}
	// It creates what it is given, at 0700.
	dir := filepath.Join(t.TempDir(), "nested", "mail")
	sink, err := NewDevSink(dir)
	if err != nil {
		t.Fatalf("NewDevSink: %v", err)
	}
	info, err := os.Stat(sink.dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory mode = %o, want 700", perm)
	}
}

func TestDevSinkHonoursContext(t *testing.T) {
	sink := testSink(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sink.Send(ctx, Mail{To: "a@example.com", Subject: "s", Text: "t", HTML: "h"}); err == nil {
		t.Error("Send on a cancelled context = nil error, want the cancellation: a shutting-down worker " +
			"must not keep writing messages")
	}
}

// recommendedSinkDir is the location a development setup points
// POST_MAIL_SINK_DIR at — the value `.env.example` shows — relative to the
// working tree root.
const recommendedSinkDir = ".dev/mail"

// TestRecommendedSinkDirIsInvisibleToGit is the test half of the rule the
// `.dev/` entry in the repository's .gitignore is the other half of — the
// same pair, and the same reasoning, as backupdr's DefaultArtifactsRelPath
// and `.backup-dr/` (see TestDefaultArtifactDirIsActuallyGitignored). The
// path and the ignore entry live in two different files in two different
// languages, so moving either one silently is what this fails on: a worker
// left running in development would fill the directory with whole messages,
// and one `git add -A` would put notification content — the labels of
// projects and assets, which are read behind an audience gate and exist in
// the file for exactly that reason — into the history.
//
// Three things are asserted, and the third is what makes the second
// evidence rather than a coincidence:
//
//  1. the sink really writes a message at that path, and the file holds it
//     (a directory nothing was ever written to is invisible to git for a
//     reason that has nothing to do with .gitignore);
//
//  2. `git status --porcelain` says nothing about that path — the check
//
//  3. the SAME listing, asked to include ignored paths, DOES speak about it.
//     Without this, "not mentioned" would also hold for a listing that never
//     looked — the wrong directory, untracked files turned off, a path
//     spelled differently — which is a check that cannot fail and therefore
//     checks nothing.
//
// The listing is taken at the working tree root, because that is where the
// paths it prints are the ones this test builds (and where a person types
// `git add -A`). `-unormal` is passed explicitly — the mode a bare `git
// status` uses — for two reasons: it overrides a status.showUntrackedFiles=no
// in somebody's git config, which would empty the listing and make (2) pass
// vacuously; and it is the mode that reports a wholly untracked directory
// once, under its own name, which is the case statusLineNaming exists for.
func TestRecommendedSinkDirIsInvisibleToGit(t *testing.T) {
	root, ok := gitRoot(t)
	if !ok {
		// A source tree without git cannot answer the question, and a test
		// that "passed" by not asking it would be the green light this whole
		// test exists to refuse (the backupdr precedent).
		t.Skipf("not a git working tree: this test asks git whether %s is ignored, and without git there "+
			"is no ignore rule to ask about", recommendedSinkDir)
	}
	dir := filepath.Join(root, filepath.FromSlash(recommendedSinkDir))

	// The documentation half of the rule first: the path asked about below
	// must be the one a developer is told to use. A test guarding a
	// directory nobody writes to is a guard on nothing.
	if got := envExampleValue(t, root, "POST_MAIL_SINK_DIR"); got != recommendedSinkDir {
		t.Fatalf(".env.example hands developers %q for POST_MAIL_SINK_DIR, but this test only knows that "+
			"%q is invisible to git: change them together — cover the new location and point this test at "+
			"it — or the protection is about a directory nobody uses", got, recommendedSinkDir)
	}

	sink, err := NewDevSink(dir)
	if err != nil {
		t.Fatalf("NewDevSink(%s): %v", dir, err)
	}
	const body = "body of the git-invisibility probe message"
	if err := sink.Send(context.Background(), Mail{
		To: "subscriber@example.com", Subject: "POST digest: 1 update", Text: body, HTML: "<p>" + body + "</p>",
	}); err != nil {
		t.Fatalf("Send into the recommended sink directory: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	if len(entries) == 0 {
		t.Fatalf("%s holds no message after a send, so the check below would pass for a sink that writes "+
			"nowhere", dir)
	}
	written := filepath.Join(dir, entries[len(entries)-1].Name())
	raw, err := os.ReadFile(written)
	if err != nil {
		t.Fatalf("read the message that was just written: %v", err)
	}
	if !strings.Contains(string(raw), body) {
		t.Fatalf("%s does not hold the message that was sent:\n%s", written, raw)
	}
	t.Cleanup(func() {
		// Only what this test wrote: a developer's own messages, and the
		// directories holding them, stay where they are. (Both removals fail
		// harmlessly when the directory is not ours to empty.)
		if err := os.Remove(written); err != nil {
			t.Errorf("removing the probe message %s: %v", written, err)
		}
		os.Remove(dir)
		os.Remove(filepath.Dir(dir))
	})

	// The ignore file is read here for two reasons, and both are load-bearing.
	// It belongs in the failure below, where the reader needs to see the
	// rules that did NOT cover the directory — and reading it ties the go
	// command's test cache to its contents. That second part is not
	// decoration: a cached PASS survives an edit to .gitignore, so without
	// this the suite can report green from a rule that no longer exists,
	// which is the exact shape of failure this test is here to refuse.
	// Nothing else makes that tie: git opens the file in a subprocess, where
	// the test binary's file log cannot see it.
	ignoreFile, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatalf("read the repository's .gitignore: %v", err)
	}

	if line, seen := statusLineNaming(gitOutput(t, root, "status", "--porcelain", "-unormal"), recommendedSinkDir); seen {
		t.Fatalf("git reports %q for %s: a dev mail file is one `git add -A` from the history. The "+
			"repository's .gitignore must cover %s/ — the entry and this path change together (they are "+
			"the two halves of this one rule).\n\nThe rules in effect:\n\n%s",
			line, recommendedSinkDir, strings.Split(recommendedSinkDir, "/")[0], ignoreFile)
	}

	// The control: asked to include ignored paths, the same listing must
	// speak about this one. If it does not, the assertion above proved only
	// that the listing is empty.
	withIgnored := gitOutput(t, root, "status", "--porcelain", "-unormal", "--ignored=traditional")
	if line, seen := statusLineNaming(withIgnored, recommendedSinkDir); !seen {
		t.Fatalf("%s does not appear in this repository's listing even with ignored paths included, so "+
			"the listing is not looking at it and the check above cannot fail:\n%s", recommendedSinkDir, withIgnored)
	} else if !strings.HasPrefix(line, "!!") {
		t.Fatalf("%s is reported as %q even with ignored paths included — the listing does not treat it as "+
			"ignored", recommendedSinkDir, line)
	}
}

// gitRoot returns the working tree's root, and whether there is one.
func gitRoot(t *testing.T) (string, bool) {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// gitOutput runs one git subcommand at root and returns its standard output.
// A git that could not answer is the test's failure, never an answer.
func gitOutput(t *testing.T, root string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", root}, args...)...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, exitErr.Stderr)
		}
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}

// statusLineNaming returns the first line of a `git status --porcelain`
// listing that speaks about rel — the path itself, something under it, or a
// directory above it — and whether there was one.
//
// The third case is the one a plain `strings.Contains` on the output misses,
// and it is the case that matters: a wholly untracked directory is reported
// once, under its own name, so a listing that has STOPPED ignoring `.dev/`
// says `?? .dev/` and never names the `.dev/mail` inside it. A substring
// search for the file's own path would keep passing while the file is one
// `git add -A` from the history.
func statusLineNaming(out, rel string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 4 {
			continue
		}
		p := strings.TrimSuffix(strings.Trim(strings.TrimSpace(line[3:]), `"`), "/")
		if i := strings.Index(p, " -> "); i >= 0 { // a rename reads "XY OLD -> NEW"
			p = strings.TrimSuffix(p[i+len(" -> "):], "/")
		}
		if p == "" {
			continue
		}
		if p == rel || strings.HasPrefix(p, rel+"/") || strings.HasPrefix(rel, p+"/") {
			return line, true
		}
	}
	return "", false
}

// envExampleValue reads the value `.env.example` shows for key, or fails the
// test when the file does not show one. The line is commented out — every
// value in that file is an example, and the file is the documentation of
// what an operator sets — so a leading "#" is stripped before the key is
// looked for, and the surrounding spaces with it.
func envExampleValue(t *testing.T, root, key string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, ".env.example"))
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#"))
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf(".env.example shows no value for %s", key)
	return ""
}
