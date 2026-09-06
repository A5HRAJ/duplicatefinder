package media

// A positive control over REAL files. The validators' one unforgivable
// outcome is convicting a healthy file, and no synthetic fixture stands in
// for the variety of files a NAS actually holds: progressive and CMYK JPEGs,
// PDFs from a dozen producers, archives written by every zip tool, phone
// videos. Point DUPFINDER_CORPUS at a directory tree and this walks it,
// verifying every file, and fails on any Damaged verdict — a real file on a
// healthy disk is never damaged. Skipped without the variable, so the
// ordinary suite stays deterministic and self-contained.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestCorpusNeverConvictsARealFile(t *testing.T) {
	root := os.Getenv("DUPFINDER_CORPUS")
	if root == "" {
		t.Skip("set DUPFINDER_CORPUS to a directory of real files to run the positive control")
	}
	counts := map[string]int{} // "ext/verdict" -> n
	var damaged []string
	files := 0
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || fi.Size() == 0 || strings.HasPrefix(fi.Name(), ".") {
			return nil
		}
		files++
		open := func() (*os.File, error) { return os.Open(p) }
		st, why := VerifyContent(open, fi.Size(), nil)
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(p), "."))
		verdict := map[Intactness]string{Unproven: "unproven", Proven: "proven", Damaged: "DAMAGED"}[st]
		counts[ext+"/"+verdict]++
		if st == Damaged {
			damaged = append(damaged, fmt.Sprintf("%s: %s", p, why))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("%-24s %6d", k, counts[k])
	}
	t.Logf("%d files", files)
	for _, d := range damaged {
		t.Errorf("real file convicted: %s", d)
	}
}
