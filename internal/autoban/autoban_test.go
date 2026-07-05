package autoban

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"codex-fail-autoban/cpasdk/pluginabi"
	"codex-fail-autoban/cpasdk/pluginapi"
)

// ---- fakes -------------------------------------------------------------------

// fakeHost answers host.auth.get / host.auth.list from an in-memory table.
type fakeHost struct {
	mu      sync.Mutex
	byIndex map[string]pluginapi.HostAuthGetResponse
	entries []pluginapi.HostAuthFileEntry
	failGet bool
	calls   []string
}

func (f *fakeHost) Call(method string, request []byte) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	f.mu.Unlock()
	switch method {
	case pluginabi.MethodHostAuthGet:
		var req pluginapi.HostAuthGetRequest
		_ = json.Unmarshal(request, &req)
		if f.failGet {
			return wrapErr("not_found", "no such auth"), nil
		}
		resp, ok := f.byIndex[req.AuthIndex]
		if !ok {
			return wrapErr("not_found", "no such auth_index"), nil
		}
		return wrapOK(resp), nil
	case pluginabi.MethodHostAuthList:
		return wrapOK(map[string]any{"files": f.entries}), nil
	default:
		return wrapErr("unsupported", method), nil
	}
}

func wrapOK(v any) []byte {
	raw, _ := json.Marshal(v)
	out, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	return out
}

func wrapErr(code, msg string) []byte {
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: code, Message: msg}})
	return out
}

// fakeFiles records filesystem mutations.
type fakeFiles struct {
	mu      sync.Mutex
	removed []string
	written map[string][]byte
	failOn  string // path that should error
}

func newFakeFiles() *fakeFiles { return &fakeFiles{written: map[string][]byte{}} }

func (f *fakeFiles) Remove(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if path == f.failOn {
		return os.ErrPermission
	}
	f.removed = append(f.removed, path)
	return nil
}

func (f *fakeFiles) WriteFile(path string, data []byte, _ os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if path == f.failOn {
		return os.ErrPermission
	}
	f.written[path] = append([]byte(nil), data...)
	return nil
}

// ---- helpers -----------------------------------------------------------------

func codexCredential(t *testing.T) []byte {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{
		"type":       "codex",
		"email":      "a@b.com",
		"expired":    "2026-01-01",
		"big_number": int64(1700000000),
	})
	return raw
}

// registerHandler builds a handler with the given YAML config applied.
func registerHandler(t *testing.T, host HostAPI, files FileOps, cfgYAML string) *Handler {
	t.Helper()
	h := NewHandler(host, files, nopLogger{})
	req, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte(cfgYAML)})
	if _, err := h.Handle(pluginabi.MethodPluginRegister, req); err != nil {
		t.Fatalf("register: %v", err)
	}
	return h
}

func failedCodex(authID, authIndex string, status int, body string) []byte {
	rec := pluginapi.UsageRecord{
		Provider:  "codex",
		AuthID:    authID,
		AuthIndex: authIndex,
		Failed:    true,
		Failure:   pluginapi.UsageFailure{StatusCode: status, Body: body},
	}
	raw, _ := json.Marshal(rec)
	return raw
}

// ---- config ------------------------------------------------------------------

func TestParseConfigDefaults(t *testing.T) {
	cfg := ParseConfig(nil)
	if cfg.Mode != ModeDisable {
		t.Fatalf("default mode = %q, want disable", cfg.Mode)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0] != "codex" {
		t.Fatalf("default providers = %v", cfg.Providers)
	}
	if len(cfg.MatchStatusCodes) != 1 || cfg.MatchStatusCodes[0] != 401 {
		t.Fatalf("default status codes = %v", cfg.MatchStatusCodes)
	}
}

func TestParseConfigOverrides(t *testing.T) {
	yaml := `
enabled: true
mode: DELETE
providers: [codex, claude]
match-status-codes: [401, 403]
dry-run: true
`
	cfg := ParseConfig([]byte(yaml))
	if !cfg.Enabled {
		t.Fatal("enabled should be true")
	}
	if cfg.Mode != ModeDelete {
		t.Fatalf("mode = %q, want delete (case-insensitive)", cfg.Mode)
	}
	if strings.Join(cfg.Providers, ",") != "codex,claude" {
		t.Fatalf("providers = %v", cfg.Providers)
	}
	if len(cfg.MatchStatusCodes) != 2 || cfg.MatchStatusCodes[1] != 403 {
		t.Fatalf("status codes = %v", cfg.MatchStatusCodes)
	}
	if !cfg.DryRun {
		t.Fatal("dry-run should be true")
	}
}

func TestParseConfigScalarLists(t *testing.T) {
	// A single comma-separated scalar must be accepted for list keys.
	cfg := ParseConfig([]byte("providers: codex\nmatch-status-codes: \"401,403\"\n"))
	if strings.Join(cfg.Providers, ",") != "codex" {
		t.Fatalf("providers = %v", cfg.Providers)
	}
	if len(cfg.MatchStatusCodes) != 2 || cfg.MatchStatusCodes[0] != 401 || cfg.MatchStatusCodes[1] != 403 {
		t.Fatalf("status codes = %v", cfg.MatchStatusCodes)
	}
}

func TestParseConfigMalformedKeepsDefaults(t *testing.T) {
	cfg := ParseConfig([]byte("mode: [this is not: valid yaml"))
	if cfg.Mode != ModeDisable {
		t.Fatalf("malformed config should keep default mode, got %q", cfg.Mode)
	}
}

// ---- detection ---------------------------------------------------------------

func enabledCfg() Config {
	c := DefaultConfig()
	c.Enabled = true
	return c
}

func TestDetectStatus401(t *testing.T) {
	rec := pluginapi.UsageRecord{Provider: "codex", AuthID: "acc-1", Failed: true,
		Failure: pluginapi.UsageFailure{StatusCode: 401, Body: `{"error":{"code":"auth_unavailable","type":"authentication_error"}}`}}
	reason, ok := detect(enabledCfg(), rec)
	if !ok || !strings.Contains(reason, "401") {
		t.Fatalf("expected 401 detection, got ok=%v reason=%q", ok, reason)
	}
}

func TestDetectBodyMatchNoStatus(t *testing.T) {
	// Some failures may not carry a numeric status; body needle must still fire.
	rec := pluginapi.UsageRecord{Provider: "codex", AuthID: "acc-1", Failed: true,
		Failure: pluginapi.UsageFailure{StatusCode: 0, Body: "upstream authentication_error: token bad"}}
	if _, ok := detect(enabledCfg(), rec); !ok {
		t.Fatal("expected body-needle detection")
	}
}

func TestDetectExactUserError(t *testing.T) {
	body := `{"error":{"message":"Your authentication token has been invalidated. Please try signing in again.","type":"authentication_error","code":"auth_unavailable"}}`
	rec := pluginapi.UsageRecord{Provider: "codex", AuthID: "acc-1", Failed: true,
		Failure: pluginapi.UsageFailure{StatusCode: 401, Body: body}}
	if _, ok := detect(enabledCfg(), rec); !ok {
		t.Fatal("the exact reported error must be detected")
	}
}

func TestDetectEmptyPoolNotBanned(t *testing.T) {
	// Empty-pool auth_unavailable: no AuthID -> must never ban an account.
	rec := pluginapi.UsageRecord{Provider: "codex", AuthID: "", Failed: true,
		Failure: pluginapi.UsageFailure{StatusCode: 401, Body: `{"error":{"code":"auth_unavailable"},"message":"no auth available"}`}}
	if _, ok := detect(enabledCfg(), rec); ok {
		t.Fatal("empty-pool error (no AuthID) must not be detected")
	}
}

func TestDetectIgnoreSubstringVetoes(t *testing.T) {
	// Even with an AuthID + 401, the ignore guard for "no auth available" wins.
	rec := pluginapi.UsageRecord{Provider: "codex", AuthID: "acc-1", Failed: true,
		Failure: pluginapi.UsageFailure{StatusCode: 401, Body: "no auth available"}}
	if _, ok := detect(enabledCfg(), rec); ok {
		t.Fatal("ignore substring must veto detection")
	}
}

func TestDetectNonCodexIgnored(t *testing.T) {
	rec := pluginapi.UsageRecord{Provider: "gemini", AuthID: "acc-1", Failed: true,
		Failure: pluginapi.UsageFailure{StatusCode: 401, Body: "authentication_error"}}
	if _, ok := detect(enabledCfg(), rec); ok {
		t.Fatal("non-configured provider must be ignored")
	}
}

func TestDetectSuccessIgnored(t *testing.T) {
	rec := pluginapi.UsageRecord{Provider: "codex", AuthID: "acc-1", Failed: false,
		Failure: pluginapi.UsageFailure{StatusCode: 401}}
	if _, ok := detect(enabledCfg(), rec); ok {
		t.Fatal("successful request must be ignored")
	}
}

func TestDetectDisabledPlugin(t *testing.T) {
	rec := pluginapi.UsageRecord{Provider: "codex", AuthID: "acc-1", Failed: true,
		Failure: pluginapi.UsageFailure{StatusCode: 401, Body: "authentication_error"}}
	if _, ok := detect(DefaultConfig(), rec); ok { // Enabled=false
		t.Fatal("disabled plugin must not detect")
	}
}

// ---- setDisabledJSON ---------------------------------------------------------

func TestSetDisabledJSONPreservesFields(t *testing.T) {
	in := codexCredential(t)
	out, err := setDisabledJSON(in)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["disabled"] != true {
		t.Fatalf("disabled not set: %v", m["disabled"])
	}
	if m["email"] != "a@b.com" || m["type"] != "codex" {
		t.Fatalf("fields not preserved: %v", m)
	}
	// Integer-valued fields must not be reformatted into exponent notation.
	if !strings.Contains(string(out), "1700000000") {
		t.Fatalf("large integer reformatted: %s", out)
	}
}

// ---- full action flow --------------------------------------------------------

func newHostWith(index, path string, cred []byte) *fakeHost {
	return &fakeHost{
		byIndex: map[string]pluginapi.HostAuthGetResponse{
			index: {AuthIndex: index, Name: "codex-a.json", Path: path, JSON: cred},
		},
		entries: []pluginapi.HostAuthFileEntry{
			{ID: "acc-1", AuthIndex: index, Name: "codex-a.json", Path: path, Provider: "codex"},
		},
	}
}

func TestUsageDisableWritesDisabledFile(t *testing.T) {
	host := newHostWith("idx1", "/auth/codex-a.json", codexCredential(t))
	files := newFakeFiles()
	h := registerHandler(t, host, files, "enabled: true\nmode: disable\n")

	h.handleUsage(failedCodex("acc-1", "idx1", 401, "authentication_error"))

	data, ok := files.written["/auth/codex-a.json"]
	if !ok {
		t.Fatalf("expected disabled file write, writes=%v removes=%v", files.written, files.removed)
	}
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	if m["disabled"] != true {
		t.Fatalf("written file not disabled: %s", data)
	}
	if len(files.removed) != 0 {
		t.Fatalf("disable mode must not remove files: %v", files.removed)
	}
}

func TestUsageDeleteRemovesFile(t *testing.T) {
	host := newHostWith("idx1", "/auth/codex-a.json", codexCredential(t))
	files := newFakeFiles()
	h := registerHandler(t, host, files, "enabled: true\nmode: delete\n")

	h.handleUsage(failedCodex("acc-1", "idx1", 401, "authentication_error"))

	if len(files.removed) != 1 || files.removed[0] != "/auth/codex-a.json" {
		t.Fatalf("expected file removal, removes=%v writes=%v", files.removed, files.written)
	}
	if len(files.written) != 0 {
		t.Fatalf("delete mode must not write files: %v", files.written)
	}
}

func TestUsageDryRunTouchesNothing(t *testing.T) {
	host := newHostWith("idx1", "/auth/codex-a.json", codexCredential(t))
	files := newFakeFiles()
	h := registerHandler(t, host, files, "enabled: true\nmode: delete\ndry-run: true\n")

	h.handleUsage(failedCodex("acc-1", "idx1", 401, "authentication_error"))

	if len(files.removed) != 0 || len(files.written) != 0 {
		t.Fatalf("dry-run must not touch fs: removes=%v writes=%v", files.removed, files.written)
	}
	// But the account should still be excluded from scheduling immediately.
	if !h.excluded["acc-1"] {
		t.Fatal("dry-run should still exclude the account from scheduling")
	}
}

func TestUsageIdempotent(t *testing.T) {
	host := newHostWith("idx1", "/auth/codex-a.json", codexCredential(t))
	files := newFakeFiles()
	h := registerHandler(t, host, files, "enabled: true\nmode: delete\n")

	for i := 0; i < 3; i++ {
		h.handleUsage(failedCodex("acc-1", "idx1", 401, "authentication_error"))
	}
	if len(files.removed) != 1 {
		t.Fatalf("expected exactly one removal across repeated failures, got %d", len(files.removed))
	}
}

func TestResolveViaListFallback(t *testing.T) {
	// AuthIndex empty in the record -> resolve via host.auth.list by ID.
	host := newHostWith("idx1", "/auth/codex-a.json", codexCredential(t))
	files := newFakeFiles()
	h := registerHandler(t, host, files, "enabled: true\nmode: delete\n")

	h.handleUsage(failedCodex("acc-1", "", 401, "authentication_error"))

	if len(files.removed) != 1 {
		t.Fatalf("expected removal via list fallback, removes=%v", files.removed)
	}
}

func TestActionProviderSafetyNet(t *testing.T) {
	// Host maps the index to a NON-codex credential: the plugin must refuse.
	claudeCred, _ := json.Marshal(map[string]any{"type": "claude"})
	host := &fakeHost{byIndex: map[string]pluginapi.HostAuthGetResponse{
		"idx1": {AuthIndex: "idx1", Name: "x.json", Path: "/auth/x.json", JSON: claudeCred},
	}}
	files := newFakeFiles()
	h := registerHandler(t, host, files, "enabled: true\nmode: delete\n")

	h.handleUsage(failedCodex("acc-1", "idx1", 401, "authentication_error"))

	if len(files.removed) != 0 {
		t.Fatalf("must not delete a provider outside the configured set: %v", files.removed)
	}
	st := h.statusSnapshot()
	if len(st.Accounts) != 1 || st.Accounts[0].Error == "" {
		t.Fatalf("expected an error record, got %+v", st.Accounts)
	}
}

// ---- scheduler ---------------------------------------------------------------

func schedulerReq(ids ...string) []byte {
	cands := make([]pluginapi.SchedulerAuthCandidate, 0, len(ids))
	for i, id := range ids {
		cands = append(cands, pluginapi.SchedulerAuthCandidate{ID: id, Provider: "codex", Priority: i})
	}
	raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{Candidates: cands})
	return raw
}

func decodePick(t *testing.T, raw []byte) pluginapi.SchedulerPickResponse {
	t.Helper()
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var resp pluginapi.SchedulerPickResponse
	_ = json.Unmarshal(env.Result, &resp)
	return resp
}

func TestSchedulerDelegatesWhenNothingBanned(t *testing.T) {
	h := NewHandler(nil, newFakeFiles(), nopLogger{})
	raw, err := h.handleSchedulerPick(schedulerReq("a", "b", "c"))
	if err != nil {
		t.Fatal(err)
	}
	resp := decodePick(t, raw)
	if resp.DelegateBuiltin != pluginapi.SchedulerBuiltinRoundRobin || !resp.Handled {
		t.Fatalf("expected round-robin delegation, got %+v", resp)
	}
}

func TestSchedulerDropsBanned(t *testing.T) {
	h := NewHandler(nil, newFakeFiles(), nopLogger{})
	h.excluded["b"] = true
	raw, _ := h.handleSchedulerPick(schedulerReq("a", "b", "c")) // priorities a=0,b=1,c=2
	resp := decodePick(t, raw)
	if resp.AuthID != "c" { // highest priority survivor
		t.Fatalf("expected highest-priority survivor c, got %+v", resp)
	}
}

func TestSchedulerAllBannedDeclines(t *testing.T) {
	h := NewHandler(nil, newFakeFiles(), nopLogger{})
	h.excluded["a"] = true
	h.excluded["b"] = true
	raw, _ := h.handleSchedulerPick(schedulerReq("a", "b"))
	resp := decodePick(t, raw)
	if resp.Handled {
		t.Fatalf("expected decline (Handled=false) when all banned, got %+v", resp)
	}
}

// ---- management --------------------------------------------------------------

func TestManagementStatusAndForget(t *testing.T) {
	host := newHostWith("idx1", "/auth/codex-a.json", codexCredential(t))
	files := newFakeFiles()
	h := registerHandler(t, host, files, "enabled: true\nmode: disable\n")
	h.handleUsage(failedCodex("acc-1", "idx1", 401, "authentication_error"))

	st := h.statusSnapshot()
	if st.Count != 1 || st.Accounts[0].AuthID != "acc-1" || !st.Accounts[0].Excluded {
		t.Fatalf("unexpected status: %+v", st)
	}

	body, _ := json.Marshal(forgetRequest{AuthID: "acc-1"})
	resp := h.dispatchManagement(pluginapi.ManagementRequest{Method: "POST", Path: managementRoutePrefix + "/forget", Body: body})
	if resp.StatusCode != 200 {
		t.Fatalf("forget status = %d", resp.StatusCode)
	}
	if h.excluded["acc-1"] {
		t.Fatal("account should no longer be excluded after forget")
	}
	if len(h.statusSnapshot().Accounts) != 0 {
		t.Fatal("acted list should be empty after forget")
	}
}

func TestManagementResourcePageServed(t *testing.T) {
	h := NewHandler(nil, newFakeFiles(), nopLogger{})
	resp := h.dispatchManagement(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/resource/plugins/" + PluginName + "/status"})
	if resp.StatusCode != 200 || !strings.Contains(string(resp.Body), "codex-fail-autoban") {
		t.Fatalf("resource page not served: %d", resp.StatusCode)
	}
}

// ---- dispatch ----------------------------------------------------------------

func TestHandleRegisterReturnsCapabilities(t *testing.T) {
	h := NewHandler(nil, newFakeFiles(), nopLogger{})
	raw, err := h.Handle(pluginabi.MethodPluginRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	var env pluginabi.Envelope
	_ = json.Unmarshal(raw, &env)
	var reg registration
	_ = json.Unmarshal(env.Result, &reg)
	if !reg.Capabilities.UsagePlugin || !reg.Capabilities.Scheduler || !reg.Capabilities.ManagementAPI {
		t.Fatalf("capabilities incomplete: %+v", reg.Capabilities)
	}
	if reg.Metadata.Name != PluginName {
		t.Fatalf("metadata name = %q", reg.Metadata.Name)
	}
}

func TestHandleUnknownMethod(t *testing.T) {
	h := NewHandler(nil, newFakeFiles(), nopLogger{})
	raw, err := h.Handle("no.such.method", nil)
	if err != nil {
		t.Fatal(err)
	}
	var env pluginabi.Envelope
	_ = json.Unmarshal(raw, &env)
	if env.OK {
		t.Fatal("unknown method should produce an error envelope")
	}
}
