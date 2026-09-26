package main

import (
	"fmt"
	"net/url"

	"github.com/lichman0405/post/internal/config"
)

const insecureHTTPCookiesEnv = "POST_AUTH_ALLOW_INSECURE_HTTP"

// sessionCookiesSecure keeps the production default unless an operator
// explicitly enables a temporary HTTP trial. The override is rejected on an
// HTTPS origin and outside prod so a stale setting cannot silently weaken a
// later deployment.
func sessionCookiesSecure(layer, webOrigin, override string) (bool, error) {
	switch override {
	case "", "false":
		return layer == config.LayerProd, nil
	case "true":
		origin, err := url.Parse(webOrigin)
		if err != nil || layer != config.LayerProd || origin.Scheme != "http" || origin.Host == "" {
			return false, fmt.Errorf("%s=true requires POST_ENV=prod and an HTTP POST_WEB_ORIGIN", insecureHTTPCookiesEnv)
		}
		return false, nil
	default:
		return false, fmt.Errorf("%s must be true or false", insecureHTTPCookiesEnv)
	}
}
