package main

// The kept move report (movereport.go): a move's per-file outcome must stay
// fetchable after its own request's answer is lost, survive a restart, and
// stop being offered once the app has shown it.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func getJSON(t *testing.T, h http.HandlerFunc, method, url, body string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	w := httptest.NewRecorder()
	h(w, req)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: %v (%s)", method, url, err, w.Body.String())
	}
	return out
}

func TestMoveReportIsKeptUntilShownAndSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	s := &Server{varDir: dir, results: map[string]*toolResult{}}
	rep := &moveReport{ID: "m1", Tool: "duplicates", Dest: "/volume1/x", Total: 2,
		Moved: []string{"/volume1/a"},
		Errors: []map[string]string{{"path": "/volume1/b",
			"error": "moved, but the copy at the destination does not match the original content"}}}
	s.publishMoveReport(rep)

	st := getJSON(t, s.handleState, "GET", "/api/state", "")
	if lm, _ := st["lastMove"].(map[string]any); lm == nil || lm["id"] != "m1" {
		t.Fatalf("/state must say a report is waiting: %v", st["lastMove"])
	}
	got := getJSON(t, s.handleMoveReport, "GET", "/api/move/report?id=m1", "")
	r, _ := got["report"].(map[string]any)
	if got["found"] != true || r == nil || len(r["errors"].([]any)) != 1 ||
		!strings.Contains(r["errors"].([]any)[0].(map[string]any)["error"].(string), "does not match") {
		t.Fatalf("the report must carry every per-file issue: %v", got)
	}
	if other := getJSON(t, s.handleMoveReport, "GET", "/api/move/report?id=m0", ""); other["found"] != false {
		t.Fatal("another move's id must not be served this report")
	}
	if none := getJSON(t, s.handleMoveReport, "GET", "/api/move/report", ""); none["found"] != false {
		t.Fatal("a request with no id must not be served a report")
	}

	// A restart before the app has shown it: still waiting.
	s2 := &Server{varDir: dir, results: map[string]*toolResult{}}
	s2.loadMoveReport()
	if st2 := getJSON(t, s2.handleState, "GET", "/api/state", ""); st2["lastMove"] == nil {
		t.Fatal("the unshown report did not survive a restart")
	}

	// Shown: no longer offered, here or after another restart.
	getJSON(t, s2.handleMoveReport, "POST", "/api/move/report", `{"id":"m1"}`)
	if st3 := getJSON(t, s2.handleState, "GET", "/api/state", ""); st3["lastMove"] != nil {
		t.Fatal("a shown report is still offered")
	}
	s3 := &Server{varDir: dir, results: map[string]*toolResult{}}
	s3.loadMoveReport()
	if st4 := getJSON(t, s3.handleState, "GET", "/api/state", ""); st4["lastMove"] != nil {
		t.Fatal("a shown report came back after a restart")
	}
}

// An acknowledgement for an older move must never mark a newer move's report
// as shown.
func TestMoveReportAckForAnOlderMoveLeavesTheNewerOne(t *testing.T) {
	s := &Server{varDir: t.TempDir(), results: map[string]*toolResult{}}
	s.publishMoveReport(&moveReport{ID: "old"})
	s.publishMoveReport(&moveReport{ID: "new"})
	getJSON(t, s.handleMoveReport, "POST", "/api/move/report", `{"id":"old"}`)
	if st := getJSON(t, s.handleState, "GET", "/api/state", ""); st["lastMove"] == nil {
		t.Fatal("the newer report was marked shown by the older move's acknowledgement")
	}
}
