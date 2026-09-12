package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Layer values accepted for POST_ENV. The layer is explicit in every
// language of the monorepo; there is no default.
const (
	LayerDev  = "dev"
	LayerTest = "test"
	LayerProd = "prod"
)

// EnvLayer is the environment variable that selects the configuration layer.
const EnvLayer = "POST_ENV"

// Secret is a string whose value must never reach any output. Its String and
// JSON forms are masked; compare values via underlying string operations
// (==, len) which are unaffected.
type Secret string

// Empty reports whether the secret has no value.
func (s Secret) Empty() bool { return s == "" }

// String masks the secret on every fmt/slog path.
func (s Secret) String() string { return "***" }

// MarshalJSON masks the secret in JSON output.
func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal("***") }

// SecretURL is a URL that may embed credentials (userinfo). Its String and
// JSON forms redact the userinfo via RedactURL; callers that need to connect
// with it use Raw.
type SecretURL string

// Raw returns the underlying URL, credentials included. Only code that
// actually opens the connection should call this; never log Raw.
func (s SecretURL) Raw() string { return string(s) }

// String redacts any embedded credentials.
func (s SecretURL) String() string { return RedactURL(string(s)) }

// MarshalJSON redacts any embedded credentials.
func (s SecretURL) MarshalJSON() ([]byte, error) { return json.Marshal(RedactURL(string(s))) }

// Config is the full typed POST configuration. Field values are raw (use them
// to connect); every output path (String, slog LogValue, JSON) is redacted.
type Config struct {
	Layer       string
	Server      ServerConfig
	Database    DatabaseConfig
	Redis       RedisConfig
	Blob        BlobConfig
	GitProvider GitProviderConfig
}

// ServerConfig configures the HTTP API server.
type ServerConfig struct {
	// Addr is the HTTP listen address (POST_API_ADDR).
	Addr string
}

// DatabaseConfig configures the PostgreSQL semantic store.
type DatabaseConfig struct {
	Host     string // POST_DB_HOST
	Port     int    // POST_DB_PORT
	User     string // POST_DB_USER
	Password Secret // POST_DB_PASSWORD — required, no default in any layer
	Name     string // POST_DB_NAME
	SSLMode  string // POST_DB_SSLMODE — required, no default
}

// RedisConfig configures the cache/async store.
type RedisConfig struct {
	Addr string // POST_REDIS_ADDR (host:port)
}

// BlobConfig configures the S3-compatible blob store (MinIO locally).
type BlobConfig struct {
	Endpoint  SecretURL // POST_BLOB_ENDPOINT — URL, may embed credentials
	AccessKey Secret    // POST_BLOB_ACCESS_KEY — required, no default
	SecretKey Secret    // POST_BLOB_SECRET_KEY — required, no default
	Bucket    string    // POST_BLOB_BUCKET
	UseTLS    bool      // POST_BLOB_USE_TLS
}

// GitProviderConfig configures the internal Gitea provider adapter.
type GitProviderConfig struct {
	BaseURL SecretURL // POST_GITEA_BASE_URL — URL, may embed credentials
	Token   Secret    // POST_GITEA_TOKEN — required, no default
}

// String renders the config with every secret masked and every URL redacted.
func (c *Config) String() string {
	if c == nil {
		return "config(nil)"
	}
	return fmt.Sprintf(
		"config{layer:%s server:{addr:%s} database:{host:%s port:%d user:%s password:%s name:%s sslmode:%s} redis:{addr:%s} blob:{endpoint:%s access_key:%s secret_key:%s bucket:%s use_tls:%t} gitprovider:{base_url:%s token:%s}}",
		c.Layer, c.Server.Addr,
		c.Database.Host, c.Database.Port, c.Database.User, c.Database.Password,
		c.Database.Name, c.Database.SSLMode,
		c.Redis.Addr,
		c.Blob.Endpoint, c.Blob.AccessKey, c.Blob.SecretKey, c.Blob.Bucket, c.Blob.UseTLS,
		c.GitProvider.BaseURL, c.GitProvider.Token,
	)
}

// LogValue renders the config as a slog group with secrets masked, so that
// slog.Info("cfg", "cfg", cfg) can never leak a value at any log level.
func (c *Config) LogValue() slog.Value {
	if c == nil {
		return slog.GroupValue()
	}
	return slog.GroupValue(
		slog.String("layer", c.Layer),
		slog.Group("server", slog.String("addr", c.Server.Addr)),
		slog.Group("database",
			slog.String("host", c.Database.Host),
			slog.Int("port", c.Database.Port),
			slog.String("user", c.Database.User),
			slog.String("password", c.Database.Password.String()),
			slog.String("name", c.Database.Name),
			slog.String("sslmode", c.Database.SSLMode),
		),
		slog.Group("redis", slog.String("addr", c.Redis.Addr)),
		slog.Group("blob",
			slog.String("endpoint", c.Blob.Endpoint.String()),
			slog.String("access_key", c.Blob.AccessKey.String()),
			slog.String("secret_key", c.Blob.SecretKey.String()),
			slog.String("bucket", c.Blob.Bucket),
			slog.Bool("use_tls", c.Blob.UseTLS),
		),
		slog.Group("gitprovider",
			slog.String("base_url", c.GitProvider.BaseURL.String()),
			slog.String("token", c.GitProvider.Token.String()),
		),
	)
}

// MarshalJSON renders a redacted JSON form of the config. Raw values are
// deliberately not serialized: field access is the only way to read them.
func (c *Config) MarshalJSON() ([]byte, error) {
	if c == nil {
		return json.Marshal(nil)
	}
	return json.Marshal(struct {
		Layer       string            `json:"layer"`
		Server      ServerConfig      `json:"server"`
		Database    redactedDatabase  `json:"database"`
		Redis       RedisConfig       `json:"redis"`
		Blob        redactedBlob      `json:"blob"`
		GitProvider redactedGitProvider `json:"gitprovider"`
	}{
		Layer:  c.Layer,
		Server: c.Server,
		Database: redactedDatabase{
			Host: c.Database.Host, Port: c.Database.Port, User: c.Database.User,
			Password: c.Database.Password, Name: c.Database.Name, SSLMode: c.Database.SSLMode,
		},
		Redis: c.Redis,
		Blob: redactedBlob{
			Endpoint: c.Blob.Endpoint, AccessKey: c.Blob.AccessKey,
			SecretKey: c.Blob.SecretKey, Bucket: c.Blob.Bucket, UseTLS: c.Blob.UseTLS,
		},
		GitProvider: redactedGitProvider{
			BaseURL: c.GitProvider.BaseURL, Token: c.GitProvider.Token,
		},
	})
}

// redacted* mirrors the config structs for JSON output; the Secret/SecretURL
// field types carry the masking behaviour.
type redactedDatabase struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password Secret `json:"password"`
	Name     string `json:"name"`
	SSLMode  string `json:"sslmode"`
}

type redactedBlob struct {
	Endpoint  SecretURL `json:"endpoint"`
	AccessKey Secret    `json:"access_key"`
	SecretKey Secret    `json:"secret_key"`
	Bucket    string    `json:"bucket"`
	UseTLS    bool      `json:"use_tls"`
}

type redactedGitProvider struct {
	BaseURL SecretURL `json:"base_url"`
	Token   Secret    `json:"token"`
}

// Loader resolves configuration from an environment source plus explicit
// layer files. The zero value reads the process environment.
type Loader struct {
	// Getenv resolves an environment variable. nil means os.Getenv.
	Getenv func(string) string
}

func (l Loader) getenv(key string) string {
	if l.Getenv == nil {
		return os.Getenv(key)
	}
	return l.Getenv(key)
}

// Load builds and validates the configuration. Layer files are optional and
// explicit: each path must be named .env.<layer> for the current POST_ENV,
// otherwise Load refuses (a value must never fall back across layers).
// Process-environment values override layer-file values.
func (l Loader) Load(envFiles ...string) (*Config, error) {
	values := map[string]string{}
	var problems []Problem

	layer := strings.TrimSpace(l.getenv(EnvLayer))
	if layer == "" {
		problems = append(problems, Problem{
			Key: EnvLayer,
			Msg: "the configuration layer is not set: refusing to guess; " +
				"set " + EnvLayer + " to dev, test or prod",
			Fix: "export " + EnvLayer + "=" + LayerDev + " (see .env.example)",
		})
		return nil, &ConfigError{Problems: problems}
	}
	if layer != LayerDev && layer != LayerTest && layer != LayerProd {
		problems = append(problems, Problem{
			Key: EnvLayer,
			Msg: "unknown configuration layer " + strconv.Quote(layer) +
				"; valid layers are dev, test, prod",
			Fix: "set " + EnvLayer + " to dev, test or prod",
		})
		return nil, &ConfigError{Problems: problems}
	}

	for _, file := range envFiles {
		if err := l.applyLayerFile(values, file, layer); err != nil {
			problems = append(problems, *err)
		}
	}
	for _, spec := range fieldSpecs {
		if v, ok := l.lookup(spec.key); ok {
			values[spec.key] = v
		}
	}
	if len(problems) > 0 {
		return nil, &ConfigError{Problems: problems}
	}

	cfg := &Config{Layer: layer}
	for _, spec := range fieldSpecs {
		raw, present := nonEmpty(values, spec.key)
		switch {
		case !present && spec.required:
			problems = append(problems, Problem{
				Key: spec.key,
				Msg: "required configuration is missing in layer " + layer,
				Fix: spec.fix + " (see .env.example)",
			})
		case !present:
			if err := spec.set(cfg, spec.def, &spec); err != nil {
				problems = append(problems, *err)
			}
		default:
			if err := spec.set(cfg, raw, &spec); err != nil {
				problems = append(problems, *err)
			}
		}
	}
	if len(problems) > 0 {
		return nil, &ConfigError{Problems: problems}
	}
	return cfg, nil
}

// applyLayerFile parses one layer file into values, refusing files whose
// layer does not match the current layer.
func (l Loader) applyLayerFile(values map[string]string, file, layer string) *Problem {
	if filepath.Base(file) != ".env."+layer {
		return &Problem{
			Key: EnvLayer,
			Msg: fmt.Sprintf("refusing to load %q under layer %s: the file layer "+
				"does not match POST_ENV and cross-layer fallback is forbidden", file, layer),
			Fix: "use a file named .env." + layer + " or set " + EnvLayer + " to the file's layer",
		}
	}
	parsed, err := ParseEnvFile(file)
	if err != nil {
		return &Problem{
			Key: EnvLayer,
			Msg: fmt.Sprintf("cannot load layer file %q: %v", file, err),
			Fix: "fix " + file + " (KEY=VALUE per line; see .env.example)",
		}
	}
	for k, v := range parsed {
		values[k] = v
	}
	return nil
}

// lookup returns a process-environment value and whether the key is set.
// Set-but-empty counts as set here; the empty value is rejected later (an
// empty secret is never silently accepted).
func (l Loader) lookup(key string) (string, bool) {
	if l.Getenv == nil {
		// os.Getenv returns "" for both unset and empty; keep the
		// distinction via LookupEnv.
		return os.LookupEnv(key)
	}
	// A custom Getenv cannot express "unset"; treat empty as unset there.
	v := l.Getenv(key)
	return v, v != ""
}

// nonEmpty reports the first non-empty value for key from values, and whether
// one exists. Empty values are treated as absent — an empty secret is never
// silently accepted.
func nonEmpty(values map[string]string, key string) (string, bool) {
	v, ok := values[key]
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// fieldSpec describes one configuration variable: how to parse it, whether it
// is required, its default (never for secrets) and its remediation text.
type fieldSpec struct {
	key      string
	set      func(*Config, string, *fieldSpec) *Problem
	required bool
	def      string
	fix      string
}

var fieldSpecs = []fieldSpec{
	{key: "POST_API_ADDR", def: ":8080", fix: "set POST_API_ADDR to an HTTP listen address (e.g. :8080)",
		set: func(c *Config, v string, _ *fieldSpec) *Problem { c.Server.Addr = v; return nil }},
	{key: "POST_DB_HOST", def: "127.0.0.1", fix: "set POST_DB_HOST to the PostgreSQL host",
		set: func(c *Config, v string, _ *fieldSpec) *Problem { c.Database.Host = v; return nil }},
	{key: "POST_DB_PORT", def: "5432", fix: "set POST_DB_PORT to the PostgreSQL port (1-65535)",
		set: func(c *Config, v string, s *fieldSpec) *Problem {
			return parseInt(c, v, s, func(c *Config, n int) { c.Database.Port = n })
		}},
	{key: "POST_DB_USER", def: "postgres", fix: "set POST_DB_USER to the PostgreSQL user",
		set: func(c *Config, v string, _ *fieldSpec) *Problem { c.Database.User = v; return nil }},
	{key: "POST_DB_PASSWORD", required: true, fix: "set POST_DB_PASSWORD to the PostgreSQL password (never commit it)",
		set: func(c *Config, v string, _ *fieldSpec) *Problem { c.Database.Password = Secret(v); return nil }},
	{key: "POST_DB_NAME", def: "post", fix: "set POST_DB_NAME to the PostgreSQL database name",
		set: func(c *Config, v string, _ *fieldSpec) *Problem { c.Database.Name = v; return nil }},
	{key: "POST_DB_SSLMODE", required: true,
		fix: "set POST_DB_SSLMODE to one of disable, allow, prefer, require, verify-ca, verify-full",
		set: func(c *Config, v string, s *fieldSpec) *Problem {
			switch v {
			case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
				c.Database.SSLMode = v
				return nil
			}
			return badValue(s, v, "must be one of disable, allow, prefer, require, verify-ca, verify-full")
		}},
	{key: "POST_REDIS_ADDR", def: "127.0.0.1:6379", fix: "set POST_REDIS_ADDR to host:port",
		set: func(c *Config, v string, _ *fieldSpec) *Problem { c.Redis.Addr = v; return nil }},
	{key: "POST_BLOB_ENDPOINT", def: "http://127.0.0.1:9000",
		fix: "set POST_BLOB_ENDPOINT to the S3-compatible endpoint URL",
		set: func(c *Config, v string, s *fieldSpec) *Problem {
			if err := checkHTTPURL(v); err != nil {
				// Echo the redacted form only: the URL may embed credentials.
				return badValue(s, RedactURL(v), err.Error())
			}
			c.Blob.Endpoint = SecretURL(v)
			return nil
		}},
	{key: "POST_BLOB_ACCESS_KEY", required: true, fix: "set POST_BLOB_ACCESS_KEY to the S3 access key (never commit it)",
		set: func(c *Config, v string, _ *fieldSpec) *Problem { c.Blob.AccessKey = Secret(v); return nil }},
	{key: "POST_BLOB_SECRET_KEY", required: true, fix: "set POST_BLOB_SECRET_KEY to the S3 secret key (never commit it)",
		set: func(c *Config, v string, _ *fieldSpec) *Problem { c.Blob.SecretKey = Secret(v); return nil }},
	{key: "POST_BLOB_BUCKET", def: "post", fix: "set POST_BLOB_BUCKET to the S3 bucket name",
		set: func(c *Config, v string, _ *fieldSpec) *Problem { c.Blob.Bucket = v; return nil }},
	{key: "POST_BLOB_USE_TLS", def: "false", fix: "set POST_BLOB_USE_TLS to true or false",
		set: func(c *Config, v string, s *fieldSpec) *Problem {
			return parseBool(c, v, s, func(c *Config, b bool) { c.Blob.UseTLS = b })
		}},
	{key: "POST_GITEA_BASE_URL", def: "http://127.0.0.1:3000",
		fix: "set POST_GITEA_BASE_URL to the internal Gitea URL",
		set: func(c *Config, v string, s *fieldSpec) *Problem {
			if err := checkHTTPURL(v); err != nil {
				return badValue(s, RedactURL(v), err.Error())
			}
			c.GitProvider.BaseURL = SecretURL(v)
			return nil
		}},
	{key: "POST_GITEA_TOKEN", required: true, fix: "set POST_GITEA_TOKEN to the Gitea access token (never commit it)",
		set: func(c *Config, v string, _ *fieldSpec) *Problem { c.GitProvider.Token = Secret(v); return nil }},
}

func parseInt(c *Config, v string, s *fieldSpec, assign func(*Config, int)) *Problem {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 {
		return badValue(s, v, "must be an integer between 1 and 65535")
	}
	assign(c, n)
	return nil
}

func parseBool(c *Config, v string, s *fieldSpec, assign func(*Config, bool)) *Problem {
	b, err := strconv.ParseBool(v)
	if err != nil {
		return badValue(s, v, "must be true or false")
	}
	assign(c, b)
	return nil
}

func badValue(s *fieldSpec, value, want string) *Problem {
	return &Problem{
		Key: s.key,
		Msg: fmt.Sprintf("%s has an invalid value %s: %s", s.key, strconv.Quote(value), want),
		Fix: s.fix,
	}
}

func checkHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("must be a valid http(s) URL")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("must be an absolute http(s) URL with a host")
	}
	return nil
}
