package notifications

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DevSink is the development mail transport (this task's "dev mail sink"):
// it does not send anything — it writes each message to a file in a
// directory, as an RFC 5322 message a mail client (or `less`) can open. It
// is what the digest sender runs against until a real transport exists, and
// what a developer inspects to see exactly what a subscriber would have
// received, headers included.
//
// Two properties are deliberate:
//
//   - the FILES are the record. A dev flow that logged "email sent" and
//     dropped the body would make the one question that matters — what does
//     the message actually say — unanswerable;
//   - the file name is generated, never derived from the recipient: a path
//     built from an address would be a path-traversal surface in the one
//     component that is handed user data.
type DevSink struct {
	dir  string
	from string
	log  *slog.Logger
	now  func() time.Time
}

// DefaultMailFrom is the envelope's From header for the development
// transport. A constant, not configuration: the sink is a development tool,
// and a configurable From would be a production knob on a component that
// does not send.
const DefaultMailFrom = "POST <no-reply@post.local>"

// DevSinkOption tunes a DevSink.
type DevSinkOption func(*DevSink)

// WithDevSinkLogger sets the logger (default slog.Default()).
func WithDevSinkLogger(log *slog.Logger) DevSinkOption {
	return func(s *DevSink) { s.log = log }
}

// WithDevSinkClock replaces the clock (tests: the file name and the Date
// header come from it).
func WithDevSinkClock(now func() time.Time) DevSinkOption {
	return func(s *DevSink) { s.now = now }
}

// NewDevSink builds the sink over dir, creating the directory (0700: the
// messages carry notification content) when it does not exist. An empty
// directory is an error rather than a silent no-op — a sink that accepted
// messages and wrote them nowhere would look exactly like a working one.
func NewDevSink(dir string, opts ...DevSinkOption) (*DevSink, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("notifications: the dev mail sink needs a directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("notifications: dev mail sink directory %s: %w", dir, err)
	}
	s := &DevSink{dir: dir, from: DefaultMailFrom, log: slog.Default(), now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Send implements Mailer by writing the message to a new file and returning
// the path it wrote. The file is written whole (O_CREATE|O_EXCL via a
// generated name), so a retried send leaves two messages rather than one
// half-written one, and nothing already in the directory is ever touched.
func (s *DevSink) Send(ctx context.Context, m Mail) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := s.render(m)
	if err != nil {
		return err
	}
	name, err := s.fileName()
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("notifications: dev mail sink write %s: %w", path, err)
	}
	// The address is NOT logged: which account was notified is user_id
	// territory, and the sink's own file is where the address belongs.
	s.log.Info("notifications: dev mail sink wrote a message",
		"path", path, "subject", m.Subject)
	return nil
}

// render builds the RFC 5322 message: headers, then a
// multipart/alternative body carrying the text and HTML renderings of the
// same digest.
func (s *DevSink) render(m Mail) ([]byte, error) {
	to, err := headerSafe("To", m.To)
	if err != nil {
		return nil, err
	}
	subject, err := headerSafe("Subject", m.Subject)
	if err != nil {
		return nil, err
	}
	boundary, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", s.from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "Date: %s\r\n", s.now().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", boundary)
	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", boundary, m.Text)
	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/html; charset=utf-8\r\n\r\n%s\r\n", boundary, m.HTML)
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return []byte(b.String()), nil
}

// fileName is the generated name: the wall-clock time (sortable, so the
// directory reads in send order) plus random hex, so two messages written in
// the same instant cannot collide.
func (s *DevSink) fileName() (string, error) {
	suffix, err := randomHex(4)
	if err != nil {
		return "", err
	}
	return s.now().UTC().Format("20060102T150405.000000000Z") + "-" + suffix + ".eml", nil
}

// headerSafe refuses a header value containing CR/LF. A line break in an
// address or a subject is header injection — the classic way a mail
// component is made to forge a header — so it is refused at the boundary
// rather than written into the file with a note.
func headerSafe(name, value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("notifications: the %s header is empty", name)
	}
	if strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("notifications: the %s header contains a line break", name)
	}
	return value, nil
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("notifications: random id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
