// Package limiter implements in-process, per-credential request leasing.
// It knows nothing about CPA's C ABI and is unit-testable independently.
package limiter

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

const InternalHeader = "X-Cpa-Auth-Guard-Request-Id"

type Config struct {
	DefaultLimit int
	AuthLimits   map[string]int
	Strategy     string
}

// ConfigYAML is a deliberately small, dependency-free parser for our own flat
// config fields, not a general YAML parser. auth_limits_json is a quoted JSON map.
func ConfigYAML(raw []byte) (Config, error) {
	cfg := Config{AuthLimits: map[string]int{}, Strategy: "round_robin"}
	for _, line := range strings.Split(string(raw), "\n") {
		// CPA plugin-store installs inject nested store metadata. Ignore nested keys;
		// only top-level fields belong to this plugin configuration.
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return cfg, fmt.Errorf("malformed plugin config line %q", line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		if strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") && len(value) >= 2 {
			value = strings.ReplaceAll(value[1:len(value)-1], "''", "'")
		} else if strings.HasPrefix(value, "\"") {
			var err error
			value, err = strconv.Unquote(value)
			if err != nil {
				return cfg, fmt.Errorf("bad quoted config value for %s: %w", key, err)
			}
		}
		switch key {
		case "default_limit":
			n, err := strconv.Atoi(value)
			if err != nil {
				return cfg, fmt.Errorf("default_limit must be integer: %w", err)
			}
			cfg.DefaultLimit = n
		case "auth_limits_json":
			if err := json.Unmarshal([]byte(value), &cfg.AuthLimits); err != nil {
				return cfg, fmt.Errorf("auth_limits_json must be JSON object mapping auth IDs to concurrency integers: %w", err)
			}
			if cfg.AuthLimits == nil {
				return cfg, errors.New("auth_limits_json cannot be null")
			}
		case "strategy":
			cfg.Strategy = value
		case "enabled", "priority", "store": // fields managed by CPA
		default:
			return cfg, fmt.Errorf("unknown plugin config field: %s", key)
		}
	}
	if cfg.DefaultLimit < 0 {
		return cfg, errors.New("default_limit cannot be negative")
	}
	if cfg.Strategy != "round_robin" && cfg.Strategy != "fill_first" {
		return cfg, errors.New("strategy must be round_robin or fill_first")
	}
	for id, n := range cfg.AuthLimits {
		if strings.TrimSpace(id) == "" || n < 0 {
			return cfg, fmt.Errorf("invalid limit for credential %q", id)
		}
	}
	return cfg, nil
}

type Candidate struct {
	ID       string
	Provider string
	Status   string
}

type Decision struct {
	AuthID   string
	Rejected bool
	Reason   string
	Bypass   bool
}

type State struct {
	mu  sync.Mutex
	cfg Config
	// held maps request ID to every credential reserved during all attempts.
	held      map[string]map[string]struct{}
	counts    map[string]int
	cursors   map[string]uint64
	saturated uint64
	admitted  uint64
}

func New(c Config) *State {
	return &State{cfg: c, held: map[string]map[string]struct{}{}, counts: map[string]int{}, cursors: map[string]uint64{}}
}
func (s *State) Configure(c Config) { s.mu.Lock(); defer s.mu.Unlock(); s.cfg = c }
func (s *State) Pick(requestID, provider, model string, candidates []Candidate) Decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(candidates) == 0 {
		return Decision{Bypass: true}
	}
	if requestID == "" {
		// If a request is outside the normal HTTP lifecycle, we cannot safely hold a
		// slot. Reject rather than silently violating a configured auth limit.
		for _, candidate := range candidates {
			if s.limit(candidate.ID) > 0 {
				return Decision{Rejected: true, Reason: "request ID missing; cannot enforce credential concurrency"}
			}
		}
		return Decision{Bypass: true}
	}
	key := provider + "\x00" + model
	start := 0
	if s.cfg.Strategy == "round_robin" {
		start = int(s.cursors[key] % uint64(len(candidates)))
	}
	for i := 0; i < len(candidates); i++ {
		candidate := candidates[(start+i)%len(candidates)]
		if candidate.ID == "" {
			continue
		}
		if existing := s.held[requestID]; existing != nil {
			if _, ok := existing[candidate.ID]; ok {
				s.advance(key, start, i, len(candidates))
				return Decision{AuthID: candidate.ID}
			}
		}
		max := s.limit(candidate.ID)
		if max > 0 && s.counts[candidate.ID] >= max {
			continue
		}
		if s.held[requestID] == nil {
			s.held[requestID] = map[string]struct{}{}
		}
		s.held[requestID][candidate.ID] = struct{}{}
		s.counts[candidate.ID]++
		s.admitted++
		s.advance(key, start, i, len(candidates))
		return Decision{AuthID: candidate.ID}
	}
	s.saturated++
	return Decision{Rejected: true, Reason: "all eligible credentials are at their concurrency limits"}
}
func (s *State) limit(id string) int {
	n, ok := s.cfg.AuthLimits[id]
	if ok {
		return n
	}
	return s.cfg.DefaultLimit
}
func (s *State) advance(key string, start, i, n int) {
	if s.cfg.Strategy == "round_robin" {
		s.cursors[key] = uint64((start + i + 1) % n)
	}
}
func (s *State) Complete(requestID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.held[requestID] {
		if s.counts[id] > 0 {
			s.counts[id]--
			if s.counts[id] == 0 {
				delete(s.counts, id)
			}
		}
	}
	delete(s.held, requestID)
}
func (s *State) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held = map[string]map[string]struct{}{}
	s.counts = map[string]int{}
	s.cursors = map[string]uint64{}
}
func (s *State) Snapshot() any {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := make(map[string]int, len(s.counts))
	for k, v := range s.counts {
		counts[k] = v
	}
	limits := make(map[string]int, len(s.cfg.AuthLimits))
	for k, v := range s.cfg.AuthLimits {
		limits[k] = v
	}
	return map[string]any{"active_by_auth": counts, "inflight_requests": len(s.held), "auth_limits": limits, "default_limit": s.cfg.DefaultLimit, "strategy": s.cfg.Strategy, "admitted_reservations": s.admitted, "saturated_picks": s.saturated}
}
func StampHeaders(headers http.Header, requestID string) http.Header {
	out := headers.Clone()
	if out == nil {
		out = make(http.Header)
	}
	out.Set(InternalHeader, requestID)
	return out
}
func ReadID(headers http.Header) string { return strings.TrimSpace(headers.Get(InternalHeader)) }
