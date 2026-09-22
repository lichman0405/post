package observability

import (
	"context"
	"net/http"
	"sync"
)

// Route patterns survive exactly one mux hop on their own.
//
// http.Request.Pattern is written by whichever ServeMux dispatched the
// request, on the *Request it was handed. That is enough when the middleware
// sits directly above the mux — the middleware passes one pointer down and
// reads the pattern off it when the call returns.
//
// It is NOT enough under /api/v1, where the session guard sits between the
// two muxes and rebuilds the request to attach the principal and the audit
// identity (cmd/api/authhttp/auth_middleware.go:167 "r = r.WithContext(ctx)").
// WithContext copies the Request struct, so the inner mux writes its pattern
// onto the copy — and the middleware, holding the original, sees only the
// outer mux's "/api/v1/". Every product route would collapse into one series.
//
// The fix is a cell rather than a second clone: RecordRoute reads the pattern
// off the request the inner mux actually used and deposits it in a cell that
// the middleware put in the CONTEXT. Context is carried across WithContext
// copies, so the cell is the same object on both sides of the guard, while
// the Request is not.

type routeCellKey struct{}

type routeCell struct {
	mu    sync.Mutex
	route string
}

func (c *routeCell) set(route string) {
	if route == "" {
		return
	}
	c.mu.Lock()
	c.route = route
	c.mu.Unlock()
}

func (c *routeCell) get() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.route
}

// withRouteCell installs a cell. Middleware does this once per request.
func withRouteCell(ctx context.Context) (context.Context, *routeCell) {
	cell := &routeCell{}
	return context.WithValue(ctx, routeCellKey{}, cell), cell
}

func routeCellFrom(ctx context.Context) *routeCell {
	cell, _ := ctx.Value(routeCellKey{}).(*routeCell)
	return cell
}

// RecordRoute wraps a nested mux so the pattern it matched reaches the
// request's middleware. Without it a nested route is only observable when
// nothing between the middleware and the mux rebuilt the request.
//
// It changes nothing about what the handler sees or returns: it calls through
// and, afterwards, copies one string.
func RecordRoute(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if cell := routeCellFrom(r.Context()); cell != nil {
			cell.set(r.Pattern)
		}
	})
}

// resolveRoute is the middleware's read: the innermost pattern anything
// recorded, else the pattern on the request the middleware still holds, else
// "unmatched" (a request no pattern served — the 404 case).
func resolveRoute(recorded string, r *http.Request) string {
	if recorded != "" {
		return recorded
	}
	if r.Pattern != "" {
		return r.Pattern
	}
	return "unmatched"
}
