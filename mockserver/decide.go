package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"net/http"
)

// handleDecide mirrors the platform's /decide (SPEC.md §8): every configured
// flag present in the response, inactive ones as false, evaluated with the
// frozen hashing. Secret keys are accepted only without a browser Origin —
// the same teaching-403 production uses (§7).
func (s *Server) handleDecide(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		preflight(w)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.applyFailure(w) {
		return
	}

	var req struct {
		WriteKey         string         `json:"write_key"`
		DistinctID       string         `json:"distinct_id"`
		PersonProperties map[string]any `json:"person_properties"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.WriteKey == "" || req.DistinctID == "" {
		http.Error(w, "write_key and distinct_id are required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	isPublic := s.publicKeys[req.WriteKey]
	isSecret := s.secretKeys[req.WriteKey]
	flags := s.flags
	s.mu.Unlock()

	if !isPublic && !isSecret {
		http.Error(w, "unknown write_key", http.StatusUnauthorized)
		return
	}
	if isSecret && r.Header.Get("Origin") != "" {
		http.Error(w, "secret keys must never leave your backend; call /decide with the project's public write key", http.StatusForbidden)
		return
	}

	result := map[string]any{}
	for _, f := range flags {
		result[f.Key] = evaluate(f, req.DistinctID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"flags":            result,
		"sessionRecording": map[string]any{"enabled": false, "sampleRate": 0},
	})
}

// evaluate applies the frozen rollout/variant hashing (SPEC.md §8.3). The
// mock's own tests run vectors/flag-hashing.json against these functions, so
// the mock is provably aligned with the platform. Targeting groups are out
// of scope here: the mock models rollout and variants, which is what SDKs
// can exercise without a persons database.
func evaluate(f Flag, distinctID string) any {
	if !f.Active {
		return false
	}
	if f.RolloutPercentage < 100 && bucket(f.Key, distinctID) >= float64(f.RolloutPercentage) {
		return false
	}
	if len(f.Variants) == 0 {
		return true
	}
	point := hashFraction(f.Key+":"+distinctID+":variant") * 100
	cumulative := 0.0
	for _, v := range f.Variants {
		cumulative += float64(v.RolloutPercentage)
		if point < cumulative {
			return v.Key
		}
	}
	return true
}

func bucket(flagKey, distinctID string) float64 {
	return hashFraction(flagKey+":"+distinctID) * 100
}

func hashFraction(input string) float64 {
	sum := sha256.Sum256([]byte(input))
	v := binary.BigEndian.Uint64(sum[:8])
	return float64(v) / (float64(1<<63) * 2)
}
