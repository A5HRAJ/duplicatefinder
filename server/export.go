// CSV export: POST /api/export renders a tool's stored result and uploads
// it through File Station, never overwriting an existing report.
package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
)

type ExportReq struct {
	Tool string `json:"tool"`
	Dest string `json:"dest"`
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST required")
		return
	}
	var req ExportReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad request body")
		return
	}
	destH, _, destShare, sess, ok := s.vetDestination(w, r, req.Dest)
	if !ok {
		return
	}
	defer destH.Close()
	// The upload targets the canonical (vetted) destination; destClean only
	// names the written file back to the client in the path form it sent.
	destClean := filepath.Clean(req.Dest)
	res := s.snapshotResult(req.Tool)
	if res == nil {
		writeErr(w, 400, "no results to export — run a scan first")
		return
	}
	name, content, err := exportCSV(req.Tool, res)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// The upload goes through File Station as the logged-in admin; a name
	// collision steps to the next free " (n)" name, never overwriting.
	finalName, err := exportViaFS(sess, destShare, name, content)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"file": filepath.Join(destClean, finalName)})
}

// csvCell neutralizes spreadsheet formula injection: a cell whose first
// meaningful character is =, +, -, or @ would otherwise execute as a live
// formula when the admin opens the report in Excel/LibreOffice. Importers
// skip leading whitespace and control characters inconsistently, so the
// check skips them too — "\n=HYPERLINK(…)" and " \t-2+3" are as dangerous
// as their bare forms. The leading apostrophe is the spreadsheet
// convention for "literal text".
func csvCell(s string) string {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '=' || c == '+' || c == '-' || c == '@':
			return "'" + s
		case c == ' ' || c < 0x20:
			// leading whitespace/control: keep looking for a marker
		default:
			return s
		}
	}
	return s
}

// exportCSV renders the report and returns its default filename plus the
// CSV bytes; writing (and " (n)" collision naming) happens in the File
// Station upload the handler performs. Cells that carry filesystem-derived
// strings pass through csvCell.
func exportCSV(tool string, res *toolResult) (string, []byte, error) {
	name := map[string]string{
		"duplicates":      "duplicate_report.csv",
		"empty_files":     "empty_files_report.csv",
		"temp_files":      "temp_files_report.csv",
		"empty_folders":   "empty_folders_report.csv",
		"corrupted_files": "conflicting_files_report.csv",
	}[tool]
	if name == "" {
		return "", nil, fmt.Errorf("unknown tool")
	}
	// The report is assembled in memory and handed to File Station's upload
	// as one body. That is bounded by the stored-results caps — every tool
	// now caps its stored rows, so the worst case is dupFileCap rows of a few
	// hundred bytes — and pre-sizing keeps the buffer from doubling its way
	// up to that.
	var buf bytes.Buffer
	buf.Grow(1 << 20)
	w := csv.NewWriter(&buf)
	if tool == "corrupted_files" {
		// The verdict and the reason for it are the point of this report, and
		// the export runs over the whole stored result rather than the filtered
		// view — which is why both are stored on the row at scan time instead
		// of being derived when the grid draws.
		w.Write([]string{"Set", "Name", "Location", "Size (bytes)", "Modified", "Content Hash", "Status", "Evidence"})
		for gi, g := range res.Groups {
			for _, fe := range g.Files {
				w.Write([]string{strconv.Itoa(gi + 1), csvCell(fe.Name), csvCell(fe.Dir),
					strconv.FormatInt(fe.Size, 10), fe.Mod, fe.Hash,
					csvCell(verdictLabel(fe.Verdict)), csvCell(fe.Evidence)})
			}
		}
	} else if tool == "duplicates" {
		w.Write([]string{"Group", "Hash", "Name", "Location", "Size (bytes)", "Modified", "Created", "Captured"})
		for gi, g := range res.Groups {
			for _, fe := range g.Files {
				w.Write([]string{strconv.Itoa(gi + 1), g.Hash, csvCell(fe.Name), csvCell(fe.Dir),
					strconv.FormatInt(fe.Size, 10), fe.Mod, fe.Created, csvCell(fe.Captured)})
			}
		}
	} else {
		w.Write([]string{"Name", "Location", "Size (bytes)", "Modified", "Created", "Type"})
		for _, fe := range res.Files {
			w.Write([]string{csvCell(fe.Name), csvCell(fe.Dir), strconv.FormatInt(fe.Size, 10), fe.Mod, fe.Created, csvCell(fe.Ext)})
		}
	}
	w.Flush()
	return name, buf.Bytes(), w.Error()
}
