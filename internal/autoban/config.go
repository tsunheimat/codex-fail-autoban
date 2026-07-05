package autoban

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Mode selects what the plugin does to an account after a terminal auth failure.
const (
	// ModeDisable writes disabled:true into the credential file so CPA skips it
	// (durable across restart) while leaving the file in place for later recovery.
	ModeDisable = "disable"
	// ModeDelete removes the credential file entirely.
	ModeDelete = "delete"
)

// Config is the effective, validated plugin configuration.
//
// It is produced from the plugins.configs.<pluginID> YAML subtree that CPA
// delivers verbatim inside every plugin.register / plugin.reconfigure request.
type Config struct {
	// Enabled mirrors plugins.configs.<id>.enabled. The host only invokes the
	// plugin when enabled, but the flag is honored defensively as a kill switch.
	Enabled bool
	// Mode is ModeDisable (default) or ModeDelete.
	Mode string
	// Providers is the set of provider keys (lower-cased) the plugin acts on.
	Providers []string
	// MatchStatusCodes are HTTP failure status codes that trigger a ban.
	MatchStatusCodes []int
	// MatchBodySubstrings are lower-cased needles; if the failure body contains
	// any of them the request is treated as a terminal auth failure.
	MatchBodySubstrings []string
	// IgnoreBodySubstrings are lower-cased needles that veto a ban even when a
	// status/body needle matched. It guards the empty-pool "no auth available"
	// error, which shares the auth_unavailable code but is not a per-account fault.
	IgnoreBodySubstrings []string
	// DryRun logs the decision but performs no filesystem change.
	DryRun bool
	// Debug logs, for every failed request the plugin observes, the provider,
	// auth id, status code, body, and the ban decision (with the reason it did or
	// did not act). Use it to see exactly what the plugin receives.
	Debug bool
}

// Default configuration values. They are meant to catch any per-account auth
// failure (invalidated/expired token, unauthorized, revoked oauth grant, …)
// while never firing on the unrelated empty-pool "no auth available" error.
var (
	defaultProviders        = []string{"codex"}
	defaultMatchStatusCodes = []int{401}
	// Lower-cased, auth-specific needles. CPA reclassifies Codex auth failures so
	// the body usually carries "authentication_error"/"auth_unavailable", but the
	// upstream wording varies ("invalidated oauth token", "expired", …), so match
	// broadly on auth-only phrases to avoid missing a variant.
	defaultMatchBodySubstrings = []string{
		"authentication_error", "auth_unavailable",
		"invalid_api_key", "invalid_grant", "invalid_client", "invalid_token",
		"invalidated", "oauth token", "expired token", "token expired", "token has expired",
		"invalid or expired token", "refresh_token_reused", "unauthorized",
		"sign in again", "log in again", "re-authenticate", "reauthenticate", "please sign in",
	}
	defaultIgnoreBodySubstrings = []string{"no auth available"}
)

// DefaultConfig returns the configuration used before register/reconfigure and
// whenever the config subtree is empty or unparsable.
func DefaultConfig() Config {
	return Config{
		Enabled:              false,
		Mode:                 ModeDisable,
		Providers:            append([]string(nil), defaultProviders...),
		MatchStatusCodes:     append([]int(nil), defaultMatchStatusCodes...),
		MatchBodySubstrings:  append([]string(nil), defaultMatchBodySubstrings...),
		IgnoreBodySubstrings: append([]string(nil), defaultIgnoreBodySubstrings...),
		DryRun:               false,
		Debug:                false,
	}
}

// rawConfig mirrors the YAML keys. List-valued keys accept either a YAML
// sequence ([a, b] or block list) or a single comma-separated scalar.
type rawConfig struct {
	Enabled              *bool       `yaml:"enabled"`
	Mode                 string      `yaml:"mode"`
	Providers            flexStrings `yaml:"providers"`
	MatchStatusCodes     flexInts    `yaml:"match-status-codes"`
	MatchBodySubstrings  flexStrings `yaml:"match-body-substrings"`
	IgnoreBodySubstrings flexStrings `yaml:"ignore-body-substrings"`
	DryRun               *bool       `yaml:"dry-run"`
	Debug                *bool       `yaml:"debug"`
}

// ParseConfig builds an effective Config from a plugin config YAML subtree.
// Unknown keys are ignored and any missing key falls back to its default, so a
// minimal `mode: delete` config is valid.
func ParseConfig(configYAML []byte) Config {
	cfg := DefaultConfig()
	if len(strings.TrimSpace(string(configYAML))) == 0 {
		return cfg
	}
	var raw rawConfig
	if err := yaml.Unmarshal(configYAML, &raw); err != nil {
		// Malformed config: keep safe defaults rather than acting unpredictably.
		return cfg
	}

	if raw.Enabled != nil {
		cfg.Enabled = *raw.Enabled
	}
	if mode := normalizeMode(raw.Mode); mode != "" {
		cfg.Mode = mode
	}
	if provs := lowerTrimAll(raw.Providers); len(provs) > 0 {
		cfg.Providers = provs
	}
	if len(raw.MatchStatusCodes) > 0 {
		cfg.MatchStatusCodes = dedupeInts(raw.MatchStatusCodes)
	}
	if subs := lowerTrimAll(raw.MatchBodySubstrings); len(subs) > 0 {
		cfg.MatchBodySubstrings = subs
	}
	// Ignore list is additive-by-replacement: an explicitly provided list wins,
	// otherwise the default guard remains in place.
	if subs := lowerTrimAll(raw.IgnoreBodySubstrings); len(subs) > 0 {
		cfg.IgnoreBodySubstrings = subs
	}
	if raw.DryRun != nil {
		cfg.DryRun = *raw.DryRun
	}
	if raw.Debug != nil {
		cfg.Debug = *raw.Debug
	}
	return cfg
}

func normalizeMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ModeDelete:
		return ModeDelete
	case ModeDisable:
		return ModeDisable
	default:
		return ""
	}
}

func lowerTrimAll(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, v := range in {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func dedupeInts(in []int) []int {
	out := make([]int, 0, len(in))
	seen := make(map[int]bool, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// flexStrings accepts a YAML scalar (optionally comma-separated) or a sequence.
// Parsing is lenient and never errors: an unusable element is skipped rather than
// failing the whole config (a single bad element must not silently disable the
// plugin by discarding the host-injected enabled:true).
type flexStrings []string

func (f *flexStrings) UnmarshalYAML(value *yaml.Node) error {
	*f = scalarTokens(value)
	return nil
}

// flexInts accepts a YAML scalar (optionally comma-separated) or a sequence.
// Non-integer elements are skipped rather than failing the parse (see flexStrings).
type flexInts []int

func (f *flexInts) UnmarshalYAML(value *yaml.Node) error {
	tokens := scalarTokens(value)
	out := make([]int, 0, len(tokens))
	for _, t := range tokens {
		if n, err := strconv.Atoi(t); err == nil {
			out = append(out, n)
		}
	}
	*f = out
	return nil
}

// scalarTokens flattens a YAML node into trimmed, non-empty scalar tokens. A
// scalar is comma-split; a sequence yields each scalar element; anything else
// (mappings, nested sequences) is ignored.
func scalarTokens(value *yaml.Node) []string {
	if value == nil {
		return nil
	}
	switch value.Kind {
	case yaml.ScalarNode:
		return splitCSV(value.Value)
	case yaml.SequenceNode:
		out := make([]string, 0, len(value.Content))
		for _, n := range value.Content {
			if n != nil && n.Kind == yaml.ScalarNode {
				if v := strings.TrimSpace(n.Value); v != "" {
					out = append(out, v)
				}
			}
		}
		return out
	default:
		return nil
	}
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
