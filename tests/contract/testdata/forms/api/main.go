// Package main is a synthetic registration site. It is not built, not linked
// and not imported by anything: it exists so the enumerator is exercised
// against every registration form the tree uses today plus the ones it could
// adopt tomorrow, without mutating the product tree to prove it (a check that
// can only nod at the tree it was written against is not an instrument).
//
// This file lives under testdata/ so the go tool ignores it; gofmt still sees
// it, so it must stay formatted.
package main

import (
	"fmt"
	"net/http"
)

type handler struct{ name string }

func (h handler) ServeHTTP(http.ResponseWriter, *http.Request) {}

type subRouter struct{}

func (subRouter) Routes() http.Handler { return nil }

type guarder struct{}

func (guarder) Guard(http.Handler) http.Handler { return nil }

func main() {
	mux := http.NewServeMux()
	// Form 1: method-qualified pattern via HandleFunc.
	mux.HandleFunc("POST /api/v1/things", func(http.ResponseWriter, *http.Request) {})
	// Form 2: method-qualified pattern via Handle, handler a method value.
	mux.Handle("GET /api/v1/things/{thingId}", handler{name: "get"})
	// Form 3: no method — matches every method (the exposure audit_wiring.go
	// writes in the real tree).
	mux.Handle("/api/v1/things/{thingId}/history", handler{name: "history"})
	// A guard wrapper over the whole subtree: a mount, not an endpoint.
	mux.Handle("/api/v1/", guarder{}.Guard(mux))
	// A subtree mount: also a mount, not an endpoint.
	mux.Handle("/api/v1/sub", subRouter{}.Routes())

	// Outside the contract's server prefix: enumerated, but not gated.
	mux.HandleFunc("GET /healthz", func(http.ResponseWriter, *http.Request) {})

	v1 := http.NewServeMux()
	register(v1, "cleanup")
	fmt.Println(v1, mux)
}

func register(mux *http.ServeMux, label string) {
	// The mux arrives as a parameter, not as a local: the scanner has to know
	// a *http.ServeMux parameter is a mux.
	mux.HandleFunc("DELETE /api/v1/things/{thingId}", func(http.ResponseWriter, *http.Request) {})
	fmt.Println(label)
}
