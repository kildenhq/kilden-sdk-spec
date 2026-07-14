package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

func post(t *testing.T, s *Server, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

const validEvent = `{"uuid":"0197fa10-7a2b-7c3d-8e4f-5a6b7c8d9e0f","event":"e","distinct_id":"u1","properties":{},"timestamp":"2026-07-14T12:00:00.000Z"}`

func validBatch(events ...string) string {
	if len(events) == 0 {
		events = []string{validEvent}
	}
	return fmt.Sprintf(`{"write_key":"sk_test_secret","sent_at":"2026-07-14T12:00:00.500Z","batch":[%s]}`, strings.Join(events, ","))
}

func TestCaptureAcceptsValidBatch(t *testing.T) {
	s := NewServer()
	rec := post(t, s, "/capture", validBatch(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != `{"status":"ok"}` {
		t.Fatalf("body = %q", rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/__mock/captured", nil)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var captured struct {
		Events []capturedEvent `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &captured); err != nil {
		t.Fatal(err)
	}
	if len(captured.Events) != 1 || captured.Events[0].Event != "e" {
		t.Fatalf("captured = %+v", captured)
	}
}

func TestCaptureAcceptsGzip(t *testing.T) {
	s := NewServer()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte(validBatch()))
	gz.Close()

	req := httptest.NewRequest(http.MethodPost, "/capture", &buf)
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCaptureRejections(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"unknown key", strings.Replace(validBatch(), "sk_test_secret", "sk_nope", 1), 401},
		{"empty batch", `{"write_key":"sk_test_secret","sent_at":"2026-07-14T12:00:00.500Z","batch":[]}`, 400},
		{"missing sent_at", `{"write_key":"sk_test_secret","batch":[` + validEvent + `]}`, 400},
		{"bad sent_at format", `{"write_key":"sk_test_secret","sent_at":"2026-07-14T12:00:00Z","batch":[` + validEvent + `]}`, 400},
		{"unknown top-level key", `{"write_key":"sk_test_secret","sent_at":"2026-07-14T12:00:00.500Z","extra":1,"batch":[` + validEvent + `]}`, 400},
		{"bad uuid", validBatch(strings.Replace(validEvent, "0197fa10-7a2b-7c3d-8e4f-5a6b7c8d9e0f", "not-a-uuid", 1)), 400},
		{"empty event", validBatch(strings.Replace(validEvent, `"event":"e"`, `"event":""`, 1)), 400},
		{"empty distinct_id", validBatch(strings.Replace(validEvent, `"distinct_id":"u1"`, `"distinct_id":""`, 1)), 400},
		{"missing properties", validBatch(strings.Replace(validEvent, `"properties":{},`, ``, 1)), 400},
		{"properties not object", validBatch(strings.Replace(validEvent, `"properties":{}`, `"properties":[1]`, 1)), 400},
		{"bad timestamp", validBatch(strings.Replace(validEvent, "2026-07-14T12:00:00.000Z", "2026-07-14T12:00:00Z", 1)), 400},
		{"impossible timestamp", validBatch(strings.Replace(validEvent, "2026-07-14T12:00:00.000Z", "2026-13-99T12:00:00.000Z", 1)), 400},
		{"unknown event key", validBatch(strings.Replace(validEvent, `"event":"e"`, `"event":"e","surprise":1`, 1)), 400},
		{"not json", `not json`, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewServer()
			rec := post(t, s, "/capture", c.body, nil)
			if rec.Code != c.want {
				t.Fatalf("got %d, want %d: %s", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

func TestCaptureLimits(t *testing.T) {
	s := NewServer()

	long := strings.Repeat("x", 201)
	rec := post(t, s, "/capture", validBatch(strings.Replace(validEvent, `"event":"e"`, `"event":"`+long+`"`, 1)), nil)
	if rec.Code != 400 {
		t.Fatalf("oversize event name: got %d", rec.Code)
	}

	longID := strings.Repeat("x", 513)
	rec = post(t, s, "/capture", validBatch(strings.Replace(validEvent, `"distinct_id":"u1"`, `"distinct_id":"`+longID+`"`, 1)), nil)
	if rec.Code != 400 {
		t.Fatalf("oversize distinct_id: got %d", rec.Code)
	}

	events := make([]string, 1001)
	for i := range events {
		events[i] = validEvent
	}
	rec = post(t, s, "/capture", validBatch(events...), nil)
	if rec.Code != 400 {
		t.Fatalf("1001 events: got %d", rec.Code)
	}
}

func TestOriginAllowlist(t *testing.T) {
	s := NewServer()
	post(t, s, "/__mock/origins", `{"origins":["https://ok.example"]}`, nil)

	rec := post(t, s, "/capture", validBatch(), map[string]string{"Origin": "https://evil.example"})
	if rec.Code != 403 {
		t.Fatalf("bad origin: got %d", rec.Code)
	}
	rec = post(t, s, "/capture", validBatch(), map[string]string{"Origin": "https://ok.example"})
	if rec.Code != 200 {
		t.Fatalf("good origin: got %d", rec.Code)
	}
	// No Origin header always passes (server-side traffic).
	if rec := post(t, s, "/capture", validBatch(), nil); rec.Code != 200 {
		t.Fatalf("no origin: got %d", rec.Code)
	}
}

func TestFailureSimulation(t *testing.T) {
	s := NewServer()
	post(t, s, "/__mock/fail", `{"times":2,"status":429,"retry_after":3}`, nil)

	for i := range 2 {
		rec := post(t, s, "/capture", validBatch(), nil)
		if rec.Code != 429 {
			t.Fatalf("armed failure %d: got %d", i, rec.Code)
		}
		if rec.Header().Get("Retry-After") != "3" {
			t.Fatalf("missing Retry-After, got %q", rec.Header().Get("Retry-After"))
		}
	}
	if rec := post(t, s, "/capture", validBatch(), nil); rec.Code != 200 {
		t.Fatalf("after failures drained: got %d", rec.Code)
	}

	post(t, s, "/__mock/fail", `{"mode":"corrupt"}`, nil)
	rec := post(t, s, "/decide", `{"write_key":"sk_test_secret","distinct_id":"u1"}`, nil)
	if rec.Code != 200 || json.Valid(rec.Body.Bytes()) {
		t.Fatalf("corrupt mode should return 200 with garbage, got %d %q", rec.Code, rec.Body.String())
	}
}

func TestDecide(t *testing.T) {
	s := NewServer()
	post(t, s, "/__mock/flags", `{"flags":[
		{"key":"on_flag","active":true,"rollout_percentage":100},
		{"key":"off_flag","active":false,"rollout_percentage":100},
		{"key":"variant_flag_1","active":true,"rollout_percentage":100,"variants":[{"key":"control","rollout_percentage":50},{"key":"test","rollout_percentage":50}]}
	]}`, nil)

	rec := post(t, s, "/decide", `{"write_key":"sk_test_secret","distinct_id":"user_42"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Flags map[string]any `json:"flags"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Flags["on_flag"] != true || resp.Flags["off_flag"] != false {
		t.Fatalf("flags = %v", resp.Flags)
	}
	if _, ok := resp.Flags["variant_flag_1"].(string); !ok {
		t.Fatalf("variant flag should be a string, got %v", resp.Flags["variant_flag_1"])
	}

	// Secret key with a browser Origin gets the teaching 403.
	rec = post(t, s, "/decide", `{"write_key":"sk_test_secret","distinct_id":"u1"}`, map[string]string{"Origin": "https://app.example"})
	if rec.Code != 403 {
		t.Fatalf("secret key with Origin: got %d", rec.Code)
	}
	// Public key with Origin is fine.
	rec = post(t, s, "/decide", `{"write_key":"wk_test_public","distinct_id":"u1"}`, map[string]string{"Origin": "https://app.example"})
	if rec.Code != 200 {
		t.Fatalf("public key with Origin: got %d", rec.Code)
	}
	if rec := post(t, s, "/decide", `{"write_key":"nope","distinct_id":"u1"}`, nil); rec.Code != 401 {
		t.Fatalf("unknown key: got %d", rec.Code)
	}
}

// TestHashingVectors proves the mock's evaluation is byte-aligned with the
// platform: it replays every frozen vector from vectors/flag-hashing.json.
func TestHashingVectors(t *testing.T) {
	raw, err := os.ReadFile("../vectors/flag-hashing.json")
	if err != nil {
		t.Skipf("vectors not present: %v", err)
	}
	var doc struct {
		Rollout []struct {
			FlagKey     string `json:"flag_key"`
			DistinctID  string `json:"distinct_id"`
			Uint64      string `json:"uint64"`
			BucketFloor int    `json:"bucket_floor"`
		} `json:"rollout"`
		Variants []struct {
			FlagKey    string    `json:"flag_key"`
			DistinctID string    `json:"distinct_id"`
			Variants   []Variant `json:"variants"`
			Expected   any       `json:"expected"`
		} `json:"variants"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Rollout) < 100 {
		t.Fatalf("suspiciously few rollout vectors: %d", len(doc.Rollout))
	}
	for _, v := range doc.Rollout {
		b := bucket(v.FlagKey, v.DistinctID)
		if int(b) != v.BucketFloor {
			t.Errorf("bucket(%q,%q) floor = %d, want %d", v.FlagKey, v.DistinctID, int(b), v.BucketFloor)
		}
		wantU, _ := strconv.ParseUint(v.Uint64, 10, 64)
		sumFrac := hashFraction(v.FlagKey + ":" + v.DistinctID)
		if gotU := uint64(sumFrac * (float64(1<<63) * 2)); gotU>>32 != wantU>>32 {
			// Only the float64-visible bits survive the round trip; compare
			// the top 32 which are always exact.
			t.Errorf("hash(%q,%q) top bits diverge", v.FlagKey, v.DistinctID)
		}
	}
	for _, v := range doc.Variants {
		f := Flag{Key: v.FlagKey, Active: true, RolloutPercentage: 100, Variants: v.Variants}
		if got := evaluate(f, v.DistinctID); got != v.Expected {
			t.Errorf("variant(%q,%q) = %v, want %v", v.FlagKey, v.DistinctID, got, v.Expected)
		}
	}
}

// TestPayloadVectorsAreServable replays every expect_event from
// vectors/payload.json (placeholders filled with valid examples) through
// /capture — the hand-authored vectors must describe accepted payloads.
func TestPayloadVectorsAreServable(t *testing.T) {
	raw, err := os.ReadFile("../vectors/payload.json")
	if err != nil {
		t.Skipf("vectors not present: %v", err)
	}
	var doc struct {
		Vectors []struct {
			Name        string         `json:"name"`
			ExpectEvent map[string]any `json:"expect_event"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	s := NewServer()
	for _, v := range doc.Vectors {
		if v.ExpectEvent == nil {
			continue
		}
		e := v.ExpectEvent
		if e["uuid"] == "<uuid_v7>" {
			e["uuid"] = "0197fa10-7a2b-7c3d-8e4f-5a6b7c8d9e0f"
		}
		if e["timestamp"] == "<iso8601_utc_ms>" {
			e["timestamp"] = "2026-07-14T12:00:00.000Z"
		}
		eventJSON, _ := json.Marshal(e)
		body := fmt.Sprintf(`{"write_key":"sk_test_secret","sent_at":"2026-07-14T12:00:00.500Z","batch":[%s]}`, eventJSON)
		rec := post(t, s, "/capture", body, nil)
		if rec.Code != 200 {
			t.Errorf("vector %s: got %d: %s", v.Name, rec.Code, rec.Body.String())
		}
	}
}
