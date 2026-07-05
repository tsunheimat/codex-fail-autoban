package autoban

import (
	"encoding/json"

	"codex-fail-autoban/cpasdk/pluginabi"
	"codex-fail-autoban/cpasdk/pluginapi"
)

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

// okEnvelope marshals v into a success envelope. It uses the SDK's pluginabi.Envelope
// so there is a single source of truth for the RPC wire shape (host.go decodes it too).
func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
}

// errorEnvelope builds a non-fatal error envelope.
func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: code, Message: message}})
	return raw
}

// ErrorEnvelope is the exported form used by the cgo shim to report ABI-level
// failures (invalid method, decode errors) back to the host.
func ErrorEnvelope(code, message string) []byte {
	return errorEnvelope(code, message)
}
