package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func (h *harness) do(method, path, body, remote string, auth bool) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if remote != "" {
		req.RemoteAddr = remote
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+h.cfg.Token)
	}
	rec := httptest.NewRecorder()
	newAPI(h.m).ServeHTTP(rec, req)
	return rec
}

func TestAPIAuthAndIPFilter(t *testing.T) {
	cfg := testConfig(t)
	cfg.AllowedIPs = []string{"10.0.0.0/8", "192.0.2.7"}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, cfg, serveBytes([]byte("x")))

	if rec := h.do("GET", "/v1/status", "", "198.51.100.1:5000", true); rec.Code != http.StatusForbidden {
		t.Errorf("foreign IP: %d, want 403", rec.Code)
	}
	if rec := h.do("GET", "/healthz", "", "10.1.2.3:5000", false); rec.Code != http.StatusOK {
		t.Errorf("healthz: %d, want 200", rec.Code)
	}
	if rec := h.do("GET", "/v1/status", "", "10.1.2.3:5000", false); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d, want 401", rec.Code)
	}
	req := httptest.NewRequest("GET", "/v1/status", nil)
	req.RemoteAddr = "192.0.2.7:1"
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	newAPI(h.m).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d, want 401", rec.Code)
	}
	if rec := h.do("GET", "/v1/status", "", "192.0.2.7:1", true); rec.Code != http.StatusOK {
		t.Errorf("valid request: %d, want 200", rec.Code)
	}
}

func TestAPICreateErrors(t *testing.T) {
	h := newHarness(t, nil, serveBytes([]byte("x")))
	for body, code := range map[string]int{
		`not json`: 400,
		`{"channel":1,"start":1767268800,"duration":210,"extra":1}`: 400,
		`{"channel":99,"start":1767268800,"duration":210}`:          400,
	} {
		if rec := h.do("POST", "/v1/jobs", body, "", true); rec.Code != code {
			t.Errorf("%s: %d, want %d", body, rec.Code, code)
		}
	}
	now := h.clock.Now().Unix()
	rec := h.do("POST", "/v1/jobs", `{"channel":1,"start":`+itoa(now-60)+`,"duration":210}`, "", true)
	var waiting map[string]any
	json.Unmarshal(rec.Body.Bytes(), &waiting)
	if rec.Code != http.StatusConflict || waiting["state"] != "waiting" || int64(waiting["available_at"].(float64)) != now+160 {
		t.Errorf("recent window: %d %v", rec.Code, waiting)
	}
}

func TestAPIJobLifecycle(t *testing.T) {
	data := randomBytes(t, 700<<10)
	h := newHarness(t, nil, serveSlowly(data, 5, 10*time.Millisecond))
	rec := h.do("POST", "/v1/jobs", `{"channel":4,"start":1767268800,"duration":210}`, "", true)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var job Job
	json.Unmarshal(rec.Body.Bytes(), &job)
	if job.ID == "" || job.State != StateQueued && job.State != StateDownloading {
		t.Fatalf("created job: %+v", job)
	}
	if rec := h.do("GET", "/v1/jobs/"+job.ID+"/video", "", "", true); rec.Code != http.StatusConflict && rec.Code != 200 {
		t.Errorf("video before ready: %d", rec.Code)
	}
	h.wait(job.ID, StateReady)

	rec = h.do("GET", "/v1/jobs/"+job.ID, "", "", true)
	json.Unmarshal(rec.Body.Bytes(), &job)
	if job.State != StateReady || job.Size != int64(len(data)) {
		t.Fatalf("ready job: %+v", job)
	}
	rec = h.do("GET", "/v1/jobs/"+job.ID+"/video", "", "", true)
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "video/mp4" || len(body) != len(data) {
		t.Fatalf("video: %d %s %d bytes", rec.Code, rec.Header().Get("Content-Type"), len(body))
	}
	req := httptest.NewRequest("GET", "/v1/jobs/"+job.ID+"/video", nil)
	req.Header.Set("Authorization", "Bearer "+h.cfg.Token)
	req.Header.Set("Range", "bytes=100-199")
	rr := httptest.NewRecorder()
	newAPI(h.m).ServeHTTP(rr, req)
	if rr.Code != http.StatusPartialContent || rr.Body.Len() != 100 {
		t.Errorf("range request: %d, %d bytes", rr.Code, rr.Body.Len())
	}
	if rec := h.do("DELETE", "/v1/jobs/"+job.ID, "", "", true); rec.Code != http.StatusNoContent {
		t.Errorf("delete: %d", rec.Code)
	}
	if rec := h.do("GET", "/v1/jobs/"+job.ID, "", "", true); rec.Code != http.StatusNotFound {
		t.Errorf("after delete: %d, want 404", rec.Code)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
