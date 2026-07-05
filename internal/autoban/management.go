package autoban

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"codex-fail-autoban/cpasdk/pluginapi"
)

// managementRegistration declares the plugin's Management API + resource routes.
func managementRegistration() pluginapi.ManagementRegistrationResponse {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{
				// Distinct suffix from the "/status" resource page below: sharing
				// "/status" makes the loose path matcher route the unauthenticated
				// resource GET into this (data-leaking) JSON handler.
				Method:      http.MethodGet,
				Path:        managementRoutePrefix + "/accounts",
				Description: "List accounts codex-fail-autoban has disabled or deleted after an auth failure.",
			},
			{
				Method:      http.MethodPost,
				Path:        managementRoutePrefix + "/forget",
				Description: "Drop accounts from the in-memory ban list so a re-authenticated credential can be scheduled again. Body: {\"auth_id\":\"...\"} or {\"all\":true}.",
			},
		},
		Resources: []pluginapi.ResourceRoute{
			{
				Path:        "/status",
				Menu:        "Codex Fail Autoban",
				Description: "View accounts disabled/deleted after an invalidated auth token.",
			},
		},
	}
}

func (h *Handler) handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	return okEnvelope(h.dispatchManagement(req))
}

func (h *Handler) dispatchManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	switch {
	// Resource (browser HTML) route is matched first: it is served WITHOUT
	// management auth, so it must never fall through into a management-API handler.
	case method == http.MethodGet && matchesResourcePath(req.Path, "/status"):
		h.mu.Lock()
		mode := h.cfg.Mode
		h.mu.Unlock()
		return htmlResponse(http.StatusOK, statusPage(mode))
	case method == http.MethodGet && matchesManagementPath(req.Path, "/accounts"):
		return jsonResponse(http.StatusOK, h.statusSnapshot())
	case method == http.MethodPost && matchesManagementPath(req.Path, "/forget"):
		return h.handleForget(req)
	default:
		return jsonResponse(http.StatusNotFound, map[string]any{
			"error":  "not_found",
			"path":   req.Path,
			"method": method,
		})
	}
}

type statusResponse struct {
	Plugin   string          `json:"plugin"`
	Version  string          `json:"version"`
	Mode     string          `json:"mode"`
	DryRun   bool            `json:"dry_run"`
	Count    int             `json:"count"`
	Accounts []accountStatus `json:"accounts"`
}

type accountStatus struct {
	AuthID    string `json:"auth_id"`
	AuthIndex string `json:"auth_index,omitempty"`
	Provider  string `json:"provider,omitempty"`
	FileName  string `json:"file_name,omitempty"`
	Path      string `json:"path,omitempty"`
	Mode      string `json:"mode"`
	Reason    string `json:"reason,omitempty"`
	DryRun    bool   `json:"dry_run,omitempty"`
	Pending   bool   `json:"pending,omitempty"`
	Error     string `json:"error,omitempty"`
	Excluded  bool   `json:"excluded"`
	At        string `json:"at,omitempty"`
}

func (h *Handler) statusSnapshot() statusResponse {
	h.mu.Lock()
	defer h.mu.Unlock()

	accounts := make([]accountStatus, 0, len(h.acted))
	for authID, rec := range h.acted {
		accounts = append(accounts, accountStatus{
			AuthID:    authID,
			AuthIndex: rec.AuthIndex,
			Provider:  rec.Provider,
			FileName:  rec.FileName,
			Path:      rec.Path,
			Mode:      rec.Mode,
			Reason:    rec.Reason,
			DryRun:    rec.DryRun,
			Pending:   rec.Pending,
			Error:     rec.Error,
			Excluded:  h.excluded[authID],
			At:        rec.At.UTC().Format(time.RFC3339),
		})
	}
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].At == accounts[j].At {
			return accounts[i].AuthID < accounts[j].AuthID
		}
		return accounts[i].At > accounts[j].At
	})
	return statusResponse{
		Plugin:   PluginName,
		Version:  PluginVersion,
		Mode:     h.cfg.Mode,
		DryRun:   h.cfg.DryRun,
		Count:    len(accounts),
		Accounts: accounts,
	}
}

type forgetRequest struct {
	AuthID string `json:"auth_id"`
	All    bool   `json:"all"`
}

func (h *Handler) handleForget(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body forgetRequest
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return jsonResponse(http.StatusBadRequest, map[string]any{
				"error":   "invalid_json",
				"message": err.Error(),
			})
		}
	}
	all := body.All || strings.EqualFold(req.Query.Get("all"), "true")
	authID := strings.TrimSpace(body.AuthID)
	if authID == "" {
		authID = strings.TrimSpace(req.Query.Get("auth_id"))
	}

	if !all && authID == "" {
		return jsonResponse(http.StatusBadRequest, map[string]any{
			"error":   "missing_auth_id",
			"message": "provide auth_id in the JSON body or query string, or set all=true",
		})
	}

	h.mu.Lock()
	removed := 0
	if all {
		removed = len(h.acted)
		h.excluded = make(map[string]bool)
		h.acted = make(map[string]actionRecord)
	} else {
		if h.excluded[authID] {
			delete(h.excluded, authID)
		}
		if _, ok := h.acted[authID]; ok {
			delete(h.acted, authID)
			removed = 1
		}
	}
	h.mu.Unlock()

	h.log.Info(PluginName+": forgot ban state", "auth_id", authID, "all", all, "removed", removed)
	return jsonResponse(http.StatusOK, map[string]any{
		"ok":      true,
		"all":     all,
		"auth_id": authID,
		"removed": removed,
		"status":  h.statusSnapshot(),
	})
}

func matchesManagementPath(path, suffix string) bool {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	if path == "" {
		return false
	}
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return strings.HasSuffix(path, managementRoutePrefix+suffix)
}

func matchesResourcePath(path, suffix string) bool {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	if path == "" {
		return false
	}
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return strings.HasSuffix(path, "/v0/resource/plugins/"+PluginName+suffix) ||
		strings.HasSuffix(path, "/plugins/"+PluginName+suffix)
}

func jsonResponse(status int, v any) pluginapi.ManagementResponse {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		status = http.StatusInternalServerError
		raw, _ = json.Marshal(map[string]any{"error": "marshal_error", "message": err.Error()})
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       raw,
	}
}

func htmlResponse(status int, body string) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       []byte(body),
	}
}
