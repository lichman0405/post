package observability

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/logging"

	"github.com/lichman0405/post/internal/config"
)

// RouteRedisLogging routes go-redis's internal logging through slog.
//
// T0006 surfaced go-redis's default behaviour: when Redis is down it prints
// its own retry/connection chatter to stderr through the stdlib log
// package, unformatted and invisible to the structured logger. This bridge
// makes every go-redis message a structured slog line with a
// component=go-redis attribute instead.
//
// Level: go-redis hands the logger one message channel without level
// information (its own LogLevel filter is set to errors). The messages are
// dependency retry/connection chatter, so they are logged at Warn — they
// must not read as application errors in the error metrics.
//
// Redaction: every message is passed through config.RedactForOutput before
// it is emitted — go-redis messages can embed a connection URL or DSN, and
// this is the same redactor the config layer uses everywhere else (no
// second redactor in this package).
func RouteRedisLogging(log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	redis.SetLogger(redisLogBridge{log: log.With("component", "go-redis")})
	redis.SetLogLevel(logging.LogLevelError)
}

// redisLogBridge adapts go-redis's internal.Logger interface (Printf) to
// slog. The concrete type is passed to redis.SetLogger, which takes the
// unexported internal.Logging interface — any type with a matching Printf
// satisfies it.
type redisLogBridge struct {
	log *slog.Logger
}

func (b redisLogBridge) Printf(ctx context.Context, format string, v ...any) {
	msg := config.RedactForOutput(fmt.Sprintf(format, v...))
	b.log.Log(ctx, slog.LevelWarn, "go-redis: "+msg)
}
