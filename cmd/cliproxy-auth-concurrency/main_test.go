package main

import (
	"encoding/json"
	"testing"
)

func rpc(t *testing.T, method string, v any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(v)
	out, err := handle(method, b)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		OK     bool
		Result map[string]any
	}
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("%s %s %v", method, out, err)
	}
	return env.Result
}
func TestPipeline(t *testing.T) {
	raw := []byte("auth_limits_json: '{\"acct-A\":2}'\nstrategy: round_robin")
	rpc(t, "plugin.register", map[string]any{"config_yaml": raw, "schema_version": 6})
	pre := rpc(t, "request.intercept_before", map[string]any{"RequestID": "r1", "Headers": map[string]any{}})
	headers := pre["Headers"]
	res := rpc(t, "scheduler.pick", map[string]any{"Provider": "codex", "Model": "gpt", "Options": map[string]any{"Headers": headers}, "Candidates": []any{map[string]string{"ID": "acct-A"}}})
	if res["AuthID"] != "acct-A" {
		t.Fatal(res)
	}
	res = rpc(t, "request.intercept_after", map[string]any{"RequestID": "r1"})
	if res["ClearHeaders"] == nil {
		t.Fatal(res)
	}
	rpc(t, "request.complete", map[string]any{"RequestID": "r1"})
	// Duplicate terminal event must not underflow.
	rpc(t, "request.complete", map[string]any{"RequestID": "r1"})
}
func TestOldSchemaRejected(t *testing.T) {
	b, _ := json.Marshal(map[string]any{"schema_version": 1})
	if _, err := handle("plugin.register", b); err == nil {
		t.Fatal("expected legacy host rejection")
	}
}

func TestManagementStatusAccountPickerWhitelisted(t *testing.T) {
	prev := authLister
	authLister = func() ([]accountPublic, error) {
		return []accountPublic{{ID: "stable-auth-a", Name: "codex-a.json", Email: "a@example.com", Provider: "codex", Status: "active"}}, nil
	}
	defer func() { authLister = prev }()
	rpc(t, "plugin.register", map[string]any{"schema_version": 6, "config_yaml": []byte("auth_limits_json: '{\"stable-auth-a\":2}'\n")})
	res := rpc(t, "management.handle", map[string]any{"Method": "GET", "Path": "/v0/management/plugins/auth-concurrency/status"})
	if res["StatusCode"] != float64(200) {
		t.Fatalf("expected HTTP 200: %#v", res)
	}
	var raw struct {
		Accounts   []accountPublic `json:"accounts"`
		AuthLimits map[string]int  `json:"auth_limits"`
	}
	encoded, err := json.Marshal(res["Body"])
	if err != nil {
		t.Fatal(err)
	}
	var body []byte
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Accounts) != 1 || raw.Accounts[0].ID != "stable-auth-a" || raw.AuthLimits["stable-auth-a"] != 2 {
		t.Fatalf("bad management response: %s", body)
	}
	if string(body) == "" || json.Valid(body) == false {
		t.Fatalf("expected valid JSON: %s", body)
	}
}

func TestHotReconfigureRetainsInFlightReservations(t *testing.T) {
	rpc(t, "plugin.register", map[string]any{"config_yaml": []byte("auth_limits_json: '{\"a\":2}'\n"), "schema_version": 6})
	for _, rid := range []string{"hot-r1", "hot-r2"} {
		rpc(t, "request.intercept_before", map[string]any{"RequestID": rid})
		res := rpc(t, "scheduler.pick", map[string]any{"Provider": "test", "Model": "m", "Options": map[string]any{"Headers": map[string]any{"X-Cpa-Auth-Guard-Request-Id": []string{rid}}}, "Candidates": []any{map[string]any{"ID": "a"}}})
		if res["AuthID"] != "a" {
			t.Fatalf("reservation failed: %#v", res)
		}
	}
	rpc(t, "plugin.reconfigure", map[string]any{"config_yaml": []byte("auth_limits_json: '{\"a\":1}'\n"), "schema_version": 6})
	res := rpc(t, "scheduler.pick", map[string]any{"Provider": "test", "Model": "m", "Options": map[string]any{"Headers": map[string]any{"X-Cpa-Auth-Guard-Request-Id": []string{"hot-r3"}}}, "Candidates": []any{map[string]any{"ID": "a"}}})
	if res["Reject"] != true {
		t.Fatalf("hot limit not enforced: %#v", res)
	}
	rpc(t, "request.complete", map[string]any{"RequestID": "hot-r1"})
	rpc(t, "request.complete", map[string]any{"RequestID": "hot-r2"})
	res = rpc(t, "scheduler.pick", map[string]any{"Provider": "test", "Model": "m", "Options": map[string]any{"Headers": map[string]any{"X-Cpa-Auth-Guard-Request-Id": []string{"hot-r3"}}}, "Candidates": []any{map[string]any{"ID": "a"}}})
	if res["AuthID"] != "a" {
		t.Fatalf("slot did not recover: %#v", res)
	}
	rpc(t, "request.complete", map[string]any{"RequestID": "hot-r3"})
}
