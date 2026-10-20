package limiter

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
)

func TestConfig(t *testing.T) {
	c, e := ConfigYAML([]byte("enabled: true\npriority: 100\nstrategy: round_robin\ndefault_limit: 0\nauth_limits_json: '{\"a\":2,\"b\":1}'"))
	if e != nil || c.AuthLimits["a"] != 2 {
		t.Fatalf("%+v %v", c, e)
	}
	for _, v := range []string{"default_limit: -1", "auth_limits_json: '{bad'", "strategy: random", "not_a_field: test"} {
		if _, err := ConfigYAML([]byte(v)); err == nil {
			t.Fatal("accepted:", v)
		}
	}
}
func TestLimitFallbackAndRelease(t *testing.T) {
	s := New(Config{AuthLimits: map[string]int{"a": 2, "b": 1}, Strategy: "round_robin"})
	c := []Candidate{{ID: "a"}, {ID: "b"}}
	for i := 0; i < 3; i++ {
		id := string(rune('0' + i))
		d := s.Pick(id, "codex", "m", c)
		if d.Rejected || d.AuthID == "" {
			t.Fatal("unexpected rejection", d)
		}
	}
	if d := s.Pick("4", "codex", "m", c); !d.Rejected {
		t.Fatalf("expected saturated, got %+v", d)
	}
	s.Complete("0")
	if d := s.Pick("4", "codex", "m", c); d.Rejected {
		t.Fatalf("failed to release: %+v", d)
	}
	s.Complete("0")
	s.Complete("4")
}
func TestDuplicateRetryConservativelyHolds(t *testing.T) {
	s := New(Config{AuthLimits: map[string]int{"a": 1, "b": 1}, Strategy: "round_robin"})
	c := []Candidate{{ID: "a"}, {ID: "b"}}
	d := s.Pick("r1", "p", "m", c)
	if d.AuthID != "a" {
		t.Fatal(d)
	}
	// Simulate the manager excluding the tried auth on retry.
	d = s.Pick("r1", "p", "m", []Candidate{{ID: "b"}})
	if d.AuthID != "b" {
		t.Fatal(d)
	}
	if d = s.Pick("r2", "p", "m", c); !d.Rejected {
		t.Fatalf("expected both leases held: %+v", d)
	}
	s.Complete("r1")
	if d = s.Pick("r2", "p", "m", c); d.Rejected {
		t.Fatalf("leak: %+v", d)
	}
}
func TestConcurrentLimit(t *testing.T) {
	s := New(Config{AuthLimits: map[string]int{"a": 2}, Strategy: "round_robin"})
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			d := s.Pick(string(rune(1000+n)), "p", "m", []Candidate{{ID: "a"}})
			if !d.Rejected {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if accepted != 2 {
		t.Fatalf("accepted %d, expected 2", accepted)
	}
}
func TestHeaderRoundtrip(t *testing.T) {
	h := StampHeaders(http.Header{"Other": {"v"}}, "id-1")
	if ReadID(h) != "id-1" || h.Get("Other") != "v" {
		t.Fatal(h)
	}
}
func TestSnapshot(t *testing.T) {
	s := New(Config{AuthLimits: map[string]int{"a": 1}, Strategy: "round_robin"})
	s.Pick("r", "p", "m", []Candidate{{ID: "a"}})
	if _, err := json.Marshal(s.Snapshot()); err != nil {
		t.Fatal(err)
	}
}
