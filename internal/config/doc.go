// Package config is the POST typed configuration baseline (T0004).
//
// Every consumer of POST configuration goes through this package:
//
//   - the configuration layer (dev/test/prod) is explicit and mandatory
//     (POST_ENV); a missing or ambiguous layer is a startup failure;
//   - required values have no defaults — a missing required value (and every
//     secret) is a startup failure that names the offending key and says what
//     to do, never a silently-accepted empty string;
//   - layer files (.env.<layer>) are explicit: the loader refuses to load a
//     file whose layer does not match POST_ENV, so a value can never fall
//     back across layers;
//   - secrets and credential-bearing URLs are redacted on every output path
//     (String, slog, JSON) — see RedactURL, Secret and SecretURL.
//
// Layering rules (see also .env.example at the repository root):
//
//  1. POST_ENV selects the layer: dev, test or prod. Nothing else.
//  2. Layer files are loaded only when the caller passes them, and only when
//     the file name matches the current layer (.env.<layer>).
//  3. Process environment overrides layer-file values.
//  4. Secrets never have defaults in any layer; dev credentials come from an
//     explicit .env.dev file, never from this package.
//
// The package is standard-library only, on purpose: configuration is loaded
// before anything else can be trusted.
package config
