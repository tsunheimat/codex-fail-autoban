package autoban

import (
	"encoding/json"

	"codex-fail-autoban/cpasdk/pluginapi"
)

// envelope is the plugin -> host RPC result wrapper.
type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// registration is the plugin.register / plugin.reconfigure result.
type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	UsagePlugin   bool `json:"usage_plugin"`
	Scheduler     bool `json:"scheduler"`
	ManagementAPI bool `json:"management_api"`
}

// okEnvelope marshals v into a success envelope.
func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

// errorEnvelope builds a non-fatal error envelope.
func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

// ErrorEnvelope is the exported form used by the cgo shim to report ABI-level
// failures (invalid method, decode errors) back to the host.
func ErrorEnvelope(code, message string) []byte {
	return errorEnvelope(code, message)
}
