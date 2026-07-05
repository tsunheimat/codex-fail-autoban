package autoban

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"codex-fail-autoban/cpasdk/pluginabi"
	"codex-fail-autoban/cpasdk/pluginapi"
)

// HostAPI is the plugin -> host reverse-call surface. The cgo layer in package
// main implements it over the cliproxy_host_api function pointers; tests provide
// an in-memory fake. The returned bytes are the raw RPC envelope
// ({"ok":true,"result":...} or {"ok":false,"error":...}).
type HostAPI interface {
	Call(method string, request []byte) ([]byte, error)
}

// FileOps abstracts the credential-file mutations so the action path is testable
// without touching a real filesystem.
type FileOps interface {
	Remove(path string) error
	WriteFile(path string, data []byte, perm os.FileMode) error
}

// Logger is the minimal leveled logger the plugin needs. slog satisfies it via a
// thin adapter in package main.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// osFileOps is the production FileOps backed by the standard library.
type osFileOps struct{}

func (osFileOps) Remove(path string) error { return os.Remove(path) }

func (osFileOps) WriteFile(path string, data []byte, perm os.FileMode) error {
	return os.WriteFile(path, data, perm)
}

// OSFileOps returns the production filesystem implementation.
func OSFileOps() FileOps { return osFileOps{} }

// nopLogger discards everything; used as a safe fallback.
type nopLogger struct{}

func (nopLogger) Info(string, ...any)  {}
func (nopLogger) Warn(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}

// decodeResult parses an RPC envelope and unmarshals its result into out.
func decodeResult(raw []byte, out any) error {
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode host envelope: %w", err)
	}
	if !env.OK {
		if env.Error != nil {
			msg := strings.TrimSpace(env.Error.Message)
			if msg == "" {
				msg = "host call failed"
			}
			return fmt.Errorf("%s: %s", env.Error.Code, msg)
		}
		return errors.New("host call failed")
	}
	if out == nil || len(env.Result) == 0 {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}

// hostAuthGet resolves a credential file (path + raw JSON) by auth index.
func hostAuthGet(host HostAPI, authIndex string) (pluginapi.HostAuthGetResponse, error) {
	var resp pluginapi.HostAuthGetResponse
	if host == nil {
		return resp, errors.New("host api unavailable")
	}
	req, err := json.Marshal(pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return resp, err
	}
	raw, err := host.Call(pluginabi.MethodHostAuthGet, req)
	if err != nil {
		return resp, err
	}
	if err = decodeResult(raw, &resp); err != nil {
		return resp, err
	}
	return resp, nil
}

// hostAuthList returns every credential the host currently knows about.
func hostAuthList(host HostAPI) ([]pluginapi.HostAuthFileEntry, error) {
	if host == nil {
		return nil, errors.New("host api unavailable")
	}
	raw, err := host.Call(pluginabi.MethodHostAuthList, []byte("{}"))
	if err != nil {
		return nil, err
	}
	var resp struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err = decodeResult(raw, &resp); err != nil {
		return nil, err
	}
	return resp.Files, nil
}
