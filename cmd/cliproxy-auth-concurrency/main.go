package main

/*
#include <stdint.h>
#include <stdlib.h>
typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);
typedef struct { uint32_t abi_version; void* host_ctx; cliproxy_host_call_fn call; cliproxy_host_free_fn free_buffer; } cliproxy_host_api;
static const cliproxy_host_api* saved_host;
static void store_host_api(const cliproxy_host_api* host) { saved_host = host; }
static int call_host_api(const char* method, const uint8_t* req, size_t len, cliproxy_buffer* response) {
    if (saved_host == NULL || saved_host->call == NULL) return 1;
    return saved_host->call(saved_host->host_ctx, method, req, len, response);
}
static void free_host_api_buffer(void* ptr, size_t len) {
    if (saved_host != NULL && saved_host->free_buffer != NULL && ptr != NULL) saved_host->free_buffer(ptr, len);
}
typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef struct { uint32_t abi_version; cliproxy_plugin_call_fn call; cliproxy_plugin_free_fn free_buffer; cliproxy_plugin_shutdown_fn shutdown; } cliproxy_plugin_api;
extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	_ "embed"
	"encoding/json"
	"errors"
	"github.com/tz1102/cliproxy-auth-concurrency/internal/limiter"
	"net/http"
	"strings"
	"sync/atomic"
	"unsafe"
)

const pluginID = "auth-concurrency"

//go:embed status.html
var statusPage string

const maxSchemaVersion = 6

var schemaVersion atomic.Uint32 // set when registering, preserving older host compatibility

var state = limiter.New(limiter.Config{AuthLimits: map[string]int{}, Strategy: "round_robin"})

type envelope struct {
	OK     bool `json:"ok"`
	Result any  `json:"result,omitempty"`
	Error  any  `json:"error,omitempty"`
}

func ok(result any) ([]byte, error) { return json.Marshal(envelope{OK: true, Result: result}) }
func fail(code, message string) []byte {
	b, _ := json.Marshal(envelope{OK: false, Error: map[string]string{"code": code, "message": message}})
	return b
}
func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = 1
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response == nil {
		return 1
	}
	response.ptr = nil
	response.len = 0
	if method == nil {
		write(response, fail("invalid_request", "missing method"))
		return 1
	}
	var b []byte
	if request != nil && requestLen > 0 {
		if uint64(requestLen) > 16<<20 {
			write(response, fail("invalid_request", "request exceeds 16MiB"))
			return 1
		}
		b = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	res, err := handle(C.GoString(method), b)
	if err != nil {
		write(response, fail("plugin_error", err.Error()))
		return 1
	}
	write(response, res)
	return 0
}
func write(dst *C.cliproxy_buffer, b []byte) {
	if dst == nil || len(b) == 0 {
		return
	}
	dst.ptr = C.CBytes(b)
	dst.len = C.size_t(len(b))
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, size C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = size
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { state.Reset() }

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

func registration() any {
	return map[string]any{
		"schema_version": schemaVersion.Load(),
		"metadata": map[string]any{"Name": pluginID, "Version": "0.2.1", "Author": "tz1102", "GitHubRepository": "https://github.com/tz1102/cliproxy-auth-concurrency", "ConfigFields": []any{
			map[string]any{"Name": "auth_limits_json", "Type": "string", "Description": "JSON object: credential AUTH ID => maximum in-flight requests. Example: {\"auth-id\":2}; 0=unlimited"},
			map[string]any{"Name": "default_limit", "Type": "integer", "Description": "Default concurrent requests per credential; 0=unlimited"},
			map[string]any{"Name": "strategy", "Type": "enum", "EnumValues": []string{"round_robin", "fill_first"}, "Description": "Credential selection among non-saturated candidates"},
		}},
		"capabilities": map[string]bool{"scheduler": true, "request_interceptor": true, "request_lifecycle_plugin": true, "management_api": true},
	}
}

type interceptRequest struct {
	RequestID string
	Headers   http.Header
	Body      []byte
}
type schedulerRequest struct {
	Provider   string
	Model      string
	Options    struct{ Headers http.Header }
	Candidates []limiter.Candidate
}
type completionRequest struct{ RequestID string }

func handle(method string, raw []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		var req lifecycleRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if req.SchemaVersion < 2 {
			return nil, errors.New("CPA host must support RPC plugin schema version 2+ (request.complete)")
		}
		cfg, err := limiter.ConfigYAML(req.ConfigYAML)
		if err != nil {
			return nil, err
		}
		state.Configure(cfg)
		version := req.SchemaVersion
		if version > maxSchemaVersion {
			version = maxSchemaVersion
		}
		schemaVersion.Store(version)
		return ok(registration())
	case "request.intercept_before":
		var req interceptRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if strings.TrimSpace(req.RequestID) == "" {
			return nil, errors.New("CPA did not provide RequestID")
		}
		return ok(map[string]any{"Headers": limiter.StampHeaders(req.Headers, req.RequestID), "Body": req.Body})
	case "request.intercept_after":
		var req interceptRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		// Important: strip the internal correlation header before executing upstream.
		return ok(map[string]any{"ClearHeaders": []string{limiter.InternalHeader}})
	case "scheduler.pick":
		var req schedulerRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		decision := state.Pick(limiter.ReadID(req.Options.Headers), req.Provider, req.Model, req.Candidates)
		if decision.Bypass {
			return ok(map[string]any{"Handled": false})
		}
		if decision.Rejected {
			return ok(map[string]any{"Handled": true, "Reject": true, "RejectCode": "auth_unavailable", "RejectReason": decision.Reason})
		}
		return ok(map[string]any{"Handled": true, "AuthID": decision.AuthID})
	case "request.complete":
		var req completionRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		state.Complete(req.RequestID)
		return ok(map[string]any{})
	case "management.register":
		return ok(map[string]any{
			"routes":    []any{map[string]string{"Method": "GET", "Path": "/plugins/auth-concurrency/status"}},
			"resources": []any{map[string]string{"Path": "/status", "Menu": "账号并发控制", "Description": "选择账号、设置并发并热更新；查看实时状态"}},
		})
	case "management.handle":
		var req struct {
			Method string
			Path   string
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		switch {
		case req.Method == "GET" && req.Path == "/v0/resource/plugins/auth-concurrency/status":
			return managementHTTP(200, "text/html; charset=utf-8", []byte(statusPage))
		case req.Method == "GET" && req.Path == "/v0/management/plugins/auth-concurrency/status":
			snapshot, err := json.Marshal(state.Snapshot())
			if err != nil {
				return nil, err
			}
			var info map[string]any
			if err := json.Unmarshal(snapshot, &info); err != nil {
				return nil, err
			}
			accounts, err := authLister()
			if err != nil {
				// Keep status functional on older hosts without host.auth.list.
				info["accounts"] = []accountPublic{}
				info["accounts_error"] = err.Error()
			} else {
				info["accounts"] = accounts
			}
			b, err := json.Marshal(info)
			if err != nil {
				return nil, err
			}
			return managementHTTP(200, "application/json; charset=utf-8", b)
		default:
			return managementHTTP(404, "application/json; charset=utf-8", []byte(`{"error":"not_found"}`))
		}

	default:
		return fail("unknown_method", "unsupported plugin method: "+method), nil
	}
}

// Host callback is only invoked from an authenticated management route. The
// unauthenticated browser resource serves static HTML and never exposes auths.
type accountPublic struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Provider    string `json:"provider"`
	Email       string `json:"email,omitempty"`
	Label       string `json:"label,omitempty"`
	Status      string `json:"status,omitempty"`
	Disabled    bool   `json:"disabled"`
	Unavailable bool   `json:"unavailable"`
}

// Injectable for isolated tests (no CPA host required).
var authLister = callHostAuthList

func callHostAuthList() ([]accountPublic, error) {
	payload, err := callHost("host.auth.list", struct{}{})
	if err != nil {
		return nil, err
	}
	var response struct {
		Files []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Provider    string `json:"provider"`
			Email       string `json:"email"`
			Label       string `json:"label"`
			Status      string `json:"status"`
			Disabled    bool   `json:"disabled"`
			Unavailable bool   `json:"unavailable"`
		} `json:"files"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		return nil, err
	}
	result := make([]accountPublic, 0, len(response.Files))
	seen := make(map[string]bool)
	for _, file := range response.Files {
		id := strings.TrimSpace(file.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		result = append(result, accountPublic{ID: id, Name: file.Name, Provider: file.Provider,
			Email: file.Email, Label: file.Label, Status: file.Status, Disabled: file.Disabled,
			Unavailable: file.Unavailable})
	}
	return result, nil
}

func callHost(method string, args any) (json.RawMessage, error) {
	input, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var req *C.uint8_t
	if len(input) > 0 {
		ptr := C.CBytes(input)
		if ptr == nil {
			return nil, errors.New("allocate host request failed")
		}
		defer C.free(ptr)
		req = (*C.uint8_t)(ptr)
	}
	var response C.cliproxy_buffer
	code := C.call_host_api(cMethod, req, C.size_t(len(input)), &response)
	if response.ptr != nil {
		defer C.free_host_api_buffer(response.ptr, response.len)
	}
	if code != 0 || response.ptr == nil || response.len == 0 || response.len > 16<<20 {
		return nil, errors.New("CPA host auth list callback unavailable")
	}
	data := C.GoBytes(response.ptr, C.int(response.len))
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	if !env.OK {
		return nil, errors.New(env.Error.Code + ": " + env.Error.Message)
	}
	return env.Result, nil
}

func managementHTTP(status int, contentType string, body []byte) ([]byte, error) {
	return ok(map[string]any{
		"StatusCode": status,
		"Headers": http.Header{
			"Content-Type":           {contentType},
			"Cache-Control":          {"no-store"},
			"X-Content-Type-Options": {"nosniff"},
		},
		"Body": body,
	})
}
