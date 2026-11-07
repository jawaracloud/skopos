package compliance

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/promqltest"
)

// TestOfficialPromQLCompliance runs the unmodified upstream Prometheus
// promqltest scripts against the Skopos evaluator. The testdata directory is a
// pinned checkout prepared by compliance.sh; without it the test skips so the
// normal module test run does not need network access.
//
// PROMETHEUS_COMPLIANCE_FILES optionally narrows the run to a comma-separated
// list of file names (for a documented reduced-scope run).
func TestOfficialPromQLCompliance(t *testing.T) {
	dir := os.Getenv("PROMETHEUS_COMPLIANCE_TESTDATA")
	if dir == "" {
		t.Skip("PROMETHEUS_COMPLIANCE_TESTDATA is not set; run test/compat/prometheus/compliance.sh")
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.test"))
	if err != nil {
		t.Fatalf("list compliance testdata: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no .test files under %s", dir)
	}
	if only := os.Getenv("PROMETHEUS_COMPLIANCE_FILES"); only != "" {
		wanted := make(map[string]struct{})
		for _, name := range strings.Split(only, ",") {
			wanted[strings.TrimSpace(name)] = struct{}{}
		}
		filtered := files[:0]
		for _, file := range files {
			if _, ok := wanted[filepath.Base(file)]; ok {
				filtered = append(filtered, file)
			}
		}
		files = filtered
		if len(files) == 0 {
			t.Fatalf("no matching .test files for PROMETHEUS_COMPLIANCE_FILES=%q", only)
		}
	}
	sort.Strings(files)
	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		t.Run(filepath.Base(file), func(t *testing.T) {
			promqltest.RunTest(t, string(content), Engine{})
		})
	}
}
