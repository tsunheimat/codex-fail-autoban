// Package autoban implements the provider-agnostic logic of the
// codex-fail-autoban CPA plugin: it observes completed requests, detects a
// terminal per-account authentication failure (an invalidated Codex token), and
// either disables or deletes the offending credential.
//
// The package is deliberately free of cgo so it can be compiled and unit-tested
// with CGO_ENABLED=0. Package main wires it to the native plugin ABI.
package autoban

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"codex-fail-autoban/cpasdk/pluginabi"
	"codex-fail-autoban/cpasdk/pluginapi"
)

// emptyResultEnvelope is the constant {"ok":true,"result":{}} reply returned on
// the hot paths (usage.handle, plugin.shutdown), precomputed to avoid re-marshaling
// it per request. The bytes are treated as read-only by callers.
var emptyResultEnvelope = mustEmptyResultEnvelope()

func mustEmptyResultEnvelope() []byte {
	raw, err := okEnvelope(map[string]any{})
	if err != nil {
		return []byte(`{"ok":true,"result":{}}`)
	}
	return raw
}

// Plugin identity. Kept here so both the registration and the management surface
// agree on the id used in route prefixes.
const (
	PluginName    = "codex-fail-autoban"
	PluginVersion = "0.1.1"

	managementRoutePrefix = "/plugins/" + PluginName
)

// actionRecord captures what the plugin did (or tried to do) to one account.
type actionRecord struct {
	AuthID    string    `json:"auth_id"`
	AuthIndex string    `json:"auth_index,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	FileName  string    `json:"file_name,omitempty"`
	Path      string    `json:"path,omitempty"`
	Mode      string    `json:"mode"`
	Reason    string    `json:"reason,omitempty"`
	DryRun    bool      `json:"dry_run,omitempty"`
	Error     string    `json:"error,omitempty"`
	Pending   bool      `json:"pending,omitempty"`
	At        time.Time `json:"at"`
}

// Handler holds plugin state across the long-lived plugin process.
type Handler struct {
	host  HostAPI
	files FileOps
	log   Logger

	mu          sync.Mutex
	cfg         Config
	excluded    map[string]bool         // auth IDs to drop from scheduler.pick this process
	acted       map[string]actionRecord // auth IDs already handled (idempotency + status)
	pickCounter uint64                  // rotates the scheduler pick across surviving candidates
}

// NewHandler builds a handler. A nil files or log is replaced with a safe default.
func NewHandler(host HostAPI, files FileOps, log Logger) *Handler {
	if files == nil {
		files = osFileOps{}
	}
	if log == nil {
		log = nopLogger{}
	}
	return &Handler{
		host:     host,
		files:    files,
		log:      log,
		cfg:      DefaultConfig(),
		excluded: make(map[string]bool),
		acted:    make(map[string]actionRecord),
	}
}

// Config returns a snapshot of the current effective configuration.
func (h *Handler) Config() Config {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg
}

// Handle is the single RPC entry point. It returns a marshaled result envelope
// for the given method. Unknown methods yield an error envelope (non-fatal).
func (h *Handler) Handle(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		h.applyLifecycleConfig(request)
		return okEnvelope(h.registration())
	case pluginabi.MethodUsageHandle:
		h.handleUsage(request)
		return emptyResultEnvelope, nil
	case pluginabi.MethodSchedulerPick:
		return h.handleSchedulerPick(request)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return h.handleManagement(request)
	case pluginabi.MethodPluginShutdown:
		return emptyResultEnvelope, nil
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// lifecycleRequest is the plugin.register / plugin.reconfigure payload. config_yaml
// is a Go []byte, so JSON delivers it base64-encoded and json.Unmarshal decodes it.
type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

func (h *Handler) applyLifecycleConfig(request []byte) {
	cfg := DefaultConfig()
	if len(request) > 0 {
		var req lifecycleRequest
		if err := json.Unmarshal(request, &req); err == nil {
			cfg = ParseConfig(req.ConfigYAML)
		} else {
			h.log.Warn(PluginName+": failed to decode lifecycle request", "error", err.Error())
		}
	}
	h.mu.Lock()
	h.cfg = cfg
	h.mu.Unlock()
	h.log.Info(PluginName+": configuration applied",
		"mode", cfg.Mode,
		"providers", strings.Join(cfg.Providers, ","),
		"dry_run", cfg.DryRun)
}

// registration declares metadata + capabilities. UsagePlugin observes failures,
// Scheduler enforces the ban immediately, ManagementAPI exposes status/recovery.
func (h *Handler) registration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             PluginName,
			Version:          PluginVersion,
			Author:           "tsunheimat",
			GitHubRepository: "https://github.com/tsunheimat/codex-fail-autoban",
			ConfigFields:     configFields(),
		},
		Capabilities: registrationCapability{
			UsagePlugin:   true,
			Scheduler:     true,
			ManagementAPI: true,
		},
	}
}

func configFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{
			Name:        "mode",
			Type:        pluginapi.ConfigFieldTypeEnum,
			EnumValues:  []string{ModeDisable, ModeDelete},
			Description: "What to do with an account whose auth token is invalidated: disable (write disabled:true, default) or delete (remove the credential file).",
		},
		{
			Name:        "providers",
			Type:        pluginapi.ConfigFieldTypeArray,
			Description: "Provider keys to act on. Default: [codex].",
		},
		{
			Name:        "match-status-codes",
			Type:        pluginapi.ConfigFieldTypeArray,
			Description: "HTTP failure status codes that trigger a ban. Default: [401].",
		},
		{
			Name:        "match-body-substrings",
			Type:        pluginapi.ConfigFieldTypeArray,
			Description: "Case-insensitive needles in the failure body that trigger a ban (e.g. authentication_error, auth_unavailable).",
		},
		{
			Name:        "ignore-body-substrings",
			Type:        pluginapi.ConfigFieldTypeArray,
			Description: "Case-insensitive needles that veto a ban. Default guards the empty-pool 'no auth available' error.",
		},
		{
			Name:        "dry-run",
			Type:        pluginapi.ConfigFieldTypeBoolean,
			Description: "When true, log the decision but do not modify or delete any credential file.",
		},
		{
			Name:        "debug",
			Type:        pluginapi.ConfigFieldTypeBoolean,
			Description: "When true, log every failed request the plugin sees (provider, auth id, status, body, decision) to help diagnose accounts that were not processed.",
		},
	}
}

// handleUsage observes one completed request and bans the account when the
// failure is a terminal per-account auth error.
func (h *Handler) handleUsage(raw []byte) {
	if len(raw) == 0 {
		return
	}
	var record pluginapi.UsageRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		h.log.Warn(PluginName+": failed to decode usage record", "error", err.Error())
		return
	}

	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()

	reason, ok := detect(cfg, record)
	if cfg.Debug && record.Failed {
		// Show exactly what the plugin received, so a missed account can be
		// diagnosed (wrong provider value, empty auth_id, no matching needle, …).
		h.log.Info(PluginName+": [debug] observed failed request",
			"provider", record.Provider,
			"auth_id", record.AuthID,
			"auth_index", record.AuthIndex,
			"status_code", record.Failure.StatusCode,
			"body", truncateForLog(record.Failure.Body, 400),
			"would_act", ok,
			"reason", reason)
	}
	if !ok {
		return
	}

	authID := strings.TrimSpace(record.AuthID)
	// Claim the account so concurrent duplicate failures do not double-act. We do
	// NOT add it to the scheduler ban here: exclusion happens only once we have
	// actually acted, so an account we cannot resolve or deliberately refuse to
	// touch is never silently held out of rotation. Retry a prior errored attempt.
	h.mu.Lock()
	if prev, seen := h.acted[authID]; seen && (prev.Error == "" || prev.Pending) {
		h.mu.Unlock()
		return
	}
	h.acted[authID] = actionRecord{
		AuthID:    authID,
		AuthIndex: strings.TrimSpace(record.AuthIndex),
		Provider:  strings.TrimSpace(record.Provider),
		Mode:      cfg.Mode,
		Reason:    reason,
		DryRun:    cfg.DryRun,
		Pending:   true,
		At:        time.Now(),
	}
	h.mu.Unlock()

	result := h.performAction(cfg, record, reason)

	h.mu.Lock()
	// Respect a concurrent /forget: only record the result (and apply the ban) if
	// the account is still tracked, so a forget that landed while we acted is not
	// resurrected. Exclude from scheduling only on a real, non-dry-run success.
	if _, stillTracked := h.acted[authID]; stillTracked {
		h.acted[authID] = result
		if result.Error == "" && !cfg.DryRun {
			h.excluded[authID] = true
		}
	}
	h.mu.Unlock()

	if result.Error != "" {
		h.log.Error(PluginName+": failed to "+cfg.Mode+" account after auth failure",
			"auth_id", authID, "reason", reason, "error", result.Error)
		return
	}
	verb := "disabled"
	if cfg.Mode == ModeDelete {
		verb = "deleted"
	}
	if cfg.DryRun {
		verb = "would " + strings.TrimSuffix(verb, "d") + " (dry-run)"
	}
	h.log.Info(PluginName+": "+verb+" account after auth failure",
		"auth_id", authID, "provider", result.Provider, "file", result.FileName, "reason", reason)
}

// performAction resolves the credential file and disables or deletes it.
func (h *Handler) performAction(cfg Config, record pluginapi.UsageRecord, reason string) actionRecord {
	result := actionRecord{
		AuthID:    strings.TrimSpace(record.AuthID),
		AuthIndex: strings.TrimSpace(record.AuthIndex),
		Provider:  strings.TrimSpace(record.Provider),
		Mode:      cfg.Mode,
		Reason:    reason,
		DryRun:    cfg.DryRun,
		At:        time.Now(),
	}

	path, rawJSON, name, fileProvider, err := h.resolveAuthFile(result.AuthIndex, result.AuthID)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Path = path
	result.FileName = name
	if fileProvider != "" {
		result.Provider = fileProvider
	}

	// Safety net (fail closed): only act on a file whose provider is confirmed to
	// be in the configured set. If the credential type cannot be read (empty or
	// typeless JSON), refuse — never delete/disable an unverifiable file.
	if !providerAllowed(cfg, fileProvider) {
		result.Error = fmt.Sprintf("resolved credential provider %q is not confirmed in the configured providers; refusing to act", fileProvider)
		return result
	}

	if cfg.DryRun {
		return result
	}

	switch cfg.Mode {
	case ModeDelete:
		if err = h.files.Remove(path); err != nil {
			result.Error = fmt.Sprintf("remove %s: %v", path, err)
		}
	default: // ModeDisable
		disabled, errSet := setDisabledJSON(rawJSON)
		if errSet != nil {
			result.Error = fmt.Sprintf("mark disabled in %s: %v", path, errSet)
			return result
		}
		if err = h.files.WriteFile(path, disabled, 0o600); err != nil {
			result.Error = fmt.Sprintf("write %s: %v", path, err)
		}
	}
	return result
}

// resolveAuthFile turns an auth index (or, as a fallback, an auth ID) into the
// credential file path, raw JSON, file name, and provider.
func (h *Handler) resolveAuthFile(authIndex, authID string) (path string, rawJSON []byte, name string, provider string, err error) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		authIndex, err = h.lookupAuthIndexByID(authID)
		if err != nil {
			return "", nil, "", "", err
		}
	}
	resp, errGet := hostAuthGet(h.host, authIndex)
	if errGet != nil {
		return "", nil, "", "", fmt.Errorf("host.auth.get: %w", errGet)
	}
	path = strings.TrimSpace(resp.Path)
	if path == "" {
		return "", nil, "", "", fmt.Errorf("host returned no file path for auth_index %s (runtime-only credential?)", authIndex)
	}
	rawJSON = []byte(resp.JSON)
	name = strings.TrimSpace(resp.Name)
	provider = providerFromCredential(rawJSON)
	return path, rawJSON, name, provider, nil
}

func (h *Handler) lookupAuthIndexByID(authID string) (string, error) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return "", fmt.Errorf("empty auth id")
	}
	entries, err := hostAuthList(h.host)
	if err != nil {
		return "", fmt.Errorf("host.auth.list: %w", err)
	}
	for _, entry := range entries {
		if entry.ID == authID && strings.TrimSpace(entry.AuthIndex) != "" {
			return strings.TrimSpace(entry.AuthIndex), nil
		}
	}
	return "", fmt.Errorf("no auth_index found for auth id %s", authID)
}

// handleSchedulerPick drops banned candidates from selection. It intervenes ONLY
// when it actually has a banned candidate to remove; otherwise it declines so the
// host applies its own configured scheduling strategy (this plugin must not impose
// a strategy on providers or requests it is not filtering).
func (h *Handler) handleSchedulerPick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}

	decline := func() ([]byte, error) {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	h.mu.Lock()
	// Fast path: nothing is banned, so there is nothing to filter.
	if len(h.excluded) == 0 {
		h.mu.Unlock()
		return decline()
	}
	available := make([]pluginapi.SchedulerAuthCandidate, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		if h.excluded[candidate.ID] {
			continue
		}
		available = append(available, candidate)
	}
	// If we dropped nothing (none of these candidates are banned) or everything
	// (all banned — let the host error/cooldown), decline and let the host choose.
	if len(available) == len(req.Candidates) || len(available) == 0 {
		h.mu.Unlock()
		return decline()
	}
	// Some candidates were banned. The ABI only lets us name a single auth or
	// delegate over the FULL (unfilterable) set, so we must pick a survivor here.
	// Rotate across survivors to preserve load-balancing instead of pinning one.
	chosen := available[int(h.pickCounter%uint64(len(available)))]
	h.pickCounter++
	h.mu.Unlock()

	return okEnvelope(pluginapi.SchedulerPickResponse{AuthID: chosen.ID, Handled: true})
}

// detect reports whether the record is a terminal per-account auth failure the
// plugin should act on. The returned reason explains the decision either way (it
// is the ban reason when ok, or the skip reason when not) — used for debug logs.
func detect(cfg Config, record pluginapi.UsageRecord) (reason string, ok bool) {
	if !cfg.Enabled {
		return "plugin disabled", false
	}
	if !record.Failed {
		return "request did not fail", false
	}
	if !providerAllowed(cfg, record.Provider) {
		return fmt.Sprintf("provider %q not in configured providers", record.Provider), false
	}
	// A specific account must have been selected. The empty-pool "no auth
	// available" error (which reuses the auth_unavailable code) carries no AuthID.
	if strings.TrimSpace(record.AuthID) == "" {
		return "no auth_id (e.g. empty-pool error)", false
	}

	// Lowercase the body once, only when there is one (a pure status failure has
	// none). The ignore veto must run before a status match can succeed.
	var body string
	if record.Failure.Body != "" {
		body = strings.ToLower(record.Failure.Body)
		for _, ignore := range cfg.IgnoreBodySubstrings {
			if ignore != "" && strings.Contains(body, ignore) {
				return fmt.Sprintf("vetoed by ignore substring %q", ignore), false
			}
		}
	}

	for _, code := range cfg.MatchStatusCodes {
		if record.Failure.StatusCode == code {
			return fmt.Sprintf("status %d", code), true
		}
	}
	if body != "" {
		for _, needle := range cfg.MatchBodySubstrings {
			if needle != "" && strings.Contains(body, needle) {
				return fmt.Sprintf("body contains %q", needle), true
			}
		}
	}
	return fmt.Sprintf("no status/body match (status=%d)", record.Failure.StatusCode), false
}

func providerAllowed(cfg Config, provider string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return false
	}
	return slices.Contains(cfg.Providers, provider)
}

// truncateForLog trims a body to a bounded, single-line form for debug logging.
func truncateForLog(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max] + "…(truncated)"
	}
	return s
}

// providerFromCredential extracts the "type" field CPA stores in every auth file.
func providerFromCredential(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(probe.Type))
}

// setDisabledJSON returns the credential JSON with "disabled": true set, leaving
// every other field's raw bytes untouched (only key order may change).
func setDisabledJSON(raw []byte) ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	fields["disabled"] = json.RawMessage("true")
	return json.Marshal(fields)
}
