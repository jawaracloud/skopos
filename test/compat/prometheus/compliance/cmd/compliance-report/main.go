// Command compliance-report turns `go test -json` output from the official
// PromQL compliance run into a pass/fail summary. It fails when a failing case
// does not match an explicitly documented unsupported category.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

const suiteTest = "TestOfficialPromQLCompliance"

type event struct {
	Action  string
	Package string
	Test    string
	Output  string
	Elapsed float64
}

type policy struct {
	PrometheusRelease string     `json:"prometheus_release"`
	PrometheusCommit  string     `json:"prometheus_commit"`
	Categories        []category `json:"categories"`
}

type category struct {
	ID         string   `json:"id"`
	Feature    string   `json:"feature"`
	Docs       string   `json:"docs"`
	Files      []string `json:"files"`
	ErrorRegex string   `json:"error_regex"`
	NameRegex  string   `json:"name_regex"`

	errorRE *regexp.Regexp
	nameRE  *regexp.Regexp
}

type testResult struct {
	Name    string
	File    string
	Action  string
	Output  string
	Elapsed float64
}

type caseResult struct {
	Name       string
	File       string
	Action     string
	Output     string
	CategoryID string
}

type fileSummary struct {
	Pass        int
	Fail        int
	Skip        int
	Unsupported int
	Unexpected  int
}

func main() {
	input := flag.String("input", "", "path to go test -json output")
	policyPath := flag.String("policy", "", "path to exceptions.json")
	markdown := flag.String("markdown", "", "path to write a Markdown report")
	flag.Parse()

	if *input == "" || *policyPath == "" {
		fatalf("usage: compliance-report -input go-test.json -policy exceptions.json [-markdown report.md]")
	}
	pol, err := loadPolicy(*policyPath)
	if err != nil {
		fatalf("load policy: %v", err)
	}
	results, err := readEvents(*input)
	if err != nil {
		fatalf("read test output: %v", err)
	}

	cases, files, totals := classify(results, pol)
	if totals.Cases == 0 {
		fatalf("no compliance test cases found in %s (did the test binary fail to build or run?)", *input)
	}

	if *markdown != "" {
		if err := writeMarkdown(*markdown, pol, totals, files, cases); err != nil {
			fatalf("write report: %v", err)
		}
	}

	fmt.Printf("Prometheus %s (%s)\n", pol.PrometheusRelease, pol.PrometheusCommit)
	fmt.Printf("cases: %d passed, %d failed (%d expected unsupported, %d unexpected), %d skipped\n",
		totals.Pass, totals.Fail, totals.Unsupported, totals.Unexpected, totals.Skip)
	for _, category := range totals.ByCategory {
		fmt.Printf("  expected unsupported %-28s %4d  %s\n", category.ID, category.Count, category.Feature)
	}
	if len(totals.UnexpectedCases) > 0 {
		fmt.Fprintf(os.Stderr, "\n%d unexpected failure(s):\n", len(totals.UnexpectedCases))
		for _, c := range totals.UnexpectedCases {
			fmt.Fprintf(os.Stderr, "\n%s\n%s\n", c.Name, indent(firstError(c.Output), "  "))
		}
		os.Exit(1)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}

func loadPolicy(path string) (*policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p policy
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	if len(p.Categories) == 0 {
		return nil, errors.New("policy has no categories")
	}
	seen := make(map[string]struct{}, len(p.Categories))
	for i := range p.Categories {
		c := &p.Categories[i]
		if c.ID == "" || c.Feature == "" {
			return nil, fmt.Errorf("category %d needs id and feature", i)
		}
		if _, ok := seen[c.ID]; ok {
			return nil, fmt.Errorf("duplicate category id %q", c.ID)
		}
		seen[c.ID] = struct{}{}
		if c.ErrorRegex != "" {
			c.errorRE, err = regexp.Compile(c.ErrorRegex)
			if err != nil {
				return nil, fmt.Errorf("category %s: bad error_regex: %w", c.ID, err)
			}
		}
		if c.NameRegex != "" {
			c.nameRE, err = regexp.Compile(c.NameRegex)
			if err != nil {
				return nil, fmt.Errorf("category %s: bad name_regex: %w", c.ID, err)
			}
		}
	}
	return &p, nil
}

func readEvents(path string) ([]testResult, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	byName := make(map[string]*testResult)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 256*1024), 8*1024*1024)
	for scanner.Scan() {
		var e event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			// Non-JSON lines can be emitted by the go tool on build failures.
			continue
		}
		if e.Test == "" {
			continue
		}
		r := byName[e.Test]
		if r == nil {
			r = &testResult{Name: e.Test}
			byName[e.Test] = r
		}
		switch e.Action {
		case "output":
			r.Output += e.Output
		case "pass", "fail", "skip":
			r.Action = e.Action
			r.Elapsed = e.Elapsed
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	out := make([]testResult, 0, len(byName))
	for _, r := range byName {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

type categoryCount struct {
	ID      string
	Feature string
	Docs    string
	Count   int
}

type totals struct {
	Cases           int
	Pass            int
	Fail            int
	Skip            int
	Unsupported     int
	Unexpected      int
	ByCategory      []categoryCount
	UnexpectedCases []caseResult
}

func classify(results []testResult, pol *policy) ([]caseResult, map[string]*fileSummary, totals) {
	var (
		cases []caseResult
		t     totals
	)
	files := make(map[string]*fileSummary)
	fileHasCaseFail := make(map[string]bool)
	fileFailed := make(map[string]testResult)

	for _, r := range results {
		parts := strings.Split(r.Name, "/")
		if len(parts) < 2 || parts[0] != suiteTest {
			continue
		}
		file := parts[1]
		summary := files[file]
		if summary == nil {
			summary = &fileSummary{}
			files[file] = summary
		}
		if len(parts) == 2 {
			if r.Action == "fail" {
				fileFailed[file] = r
			}
			continue
		}
		c := caseResult{Name: r.Name, File: file, Action: r.Action, Output: r.Output}
		switch r.Action {
		case "pass":
			summary.Pass++
			t.Pass++
		case "skip":
			summary.Skip++
			t.Skip++
		case "fail":
			summary.Fail++
			t.Fail++
			fileHasCaseFail[file] = true
			c.CategoryID = matchCategory(pol, c)
			if c.CategoryID == "" {
				summary.Unexpected++
				t.Unexpected++
				t.UnexpectedCases = append(t.UnexpectedCases, c)
			} else {
				summary.Unsupported++
				t.Unsupported++
			}
		}
		cases = append(cases, c)
	}
	t.Cases = t.Pass + t.Fail + t.Skip

	// A file can fail without any failing case event, for example when the
	// script parser rejects the file. Those are real failures, never
	// exceptions.
	for file, r := range fileFailed {
		if fileHasCaseFail[file] {
			continue
		}
		c := caseResult{Name: r.Name, File: file, Action: "fail", Output: r.Output}
		files[file].Fail++
		files[file].Unexpected++
		t.Fail++
		t.Cases++
		t.Unexpected++
		t.UnexpectedCases = append(t.UnexpectedCases, c)
		cases = append(cases, c)
	}

	byID := make(map[string]*categoryCount)
	for _, c := range cases {
		if c.CategoryID == "" {
			continue
		}
		entry := byID[c.CategoryID]
		if entry == nil {
			cat := findCategory(pol, c.CategoryID)
			entry = &categoryCount{ID: cat.ID, Feature: cat.Feature, Docs: cat.Docs}
			byID[c.CategoryID] = entry
		}
		entry.Count++
	}
	t.ByCategory = make([]categoryCount, 0, len(byID))
	for _, entry := range byID {
		t.ByCategory = append(t.ByCategory, *entry)
	}
	sort.Slice(t.ByCategory, func(i, j int) bool { return t.ByCategory[i].Count > t.ByCategory[j].Count })
	sort.Slice(t.UnexpectedCases, func(i, j int) bool { return t.UnexpectedCases[i].Name < t.UnexpectedCases[j].Name })
	return cases, files, t
}

func findCategory(pol *policy, id string) category {
	for _, c := range pol.Categories {
		if c.ID == id {
			return c
		}
	}
	return category{}
}

func matchCategory(pol *policy, c caseResult) string {
	for i := range pol.Categories {
		cat := &pol.Categories[i]
		if len(cat.Files) > 0 {
			found := false
			for _, file := range cat.Files {
				if file == c.File {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		if cat.errorRE != nil && !cat.errorRE.MatchString(c.Output) {
			continue
		}
		if cat.nameRE != nil && !cat.nameRE.MatchString(c.Name) {
			continue
		}
		return cat.ID
	}
	return ""
}

func writeMarkdown(path string, pol *policy, t totals, files map[string]*fileSummary, cases []caseResult) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# PromQL compliance run\n\n")
	fmt.Fprintf(&b, "Generated: %s\n\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "- Prometheus release: `%s`\n", pol.PrometheusRelease)
	fmt.Fprintf(&b, "- Prometheus commit: `%s`\n", pol.PrometheusCommit)
	fmt.Fprintf(&b, "- Cases: **%d passed**, **%d failed** (%d expected unsupported, %d unexpected), %d skipped\n\n",
		t.Pass, t.Fail, t.Unsupported, t.Unexpected, t.Skip)

	fmt.Fprintf(&b, "## Results by file\n\n")
	fmt.Fprintf(&b, "| File | Pass | Fail | Expected unsupported | Unexpected |\n")
	fmt.Fprintf(&b, "| --- | ---: | ---: | ---: | ---: |\n")
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f := files[name]
		fmt.Fprintf(&b, "| `%s` | %d | %d | %d | %d |\n", name, f.Pass, f.Fail, f.Unsupported, f.Unexpected)
	}

	fmt.Fprintf(&b, "\n## Expected unsupported cases\n\n")
	if len(t.ByCategory) == 0 {
		fmt.Fprintf(&b, "None.\n")
	} else {
		fmt.Fprintf(&b, "| Category | Cases | Feature | Docs |\n")
		fmt.Fprintf(&b, "| --- | ---: | --- | --- |\n")
		for _, c := range t.ByCategory {
			fmt.Fprintf(&b, "| `%s` | %d | %s | %s |\n", c.ID, c.Count, c.Feature, c.Docs)
		}
	}

	fmt.Fprintf(&b, "\n## Unexpected failures\n\n")
	if len(t.UnexpectedCases) == 0 {
		fmt.Fprintf(&b, "None. Every failing case matches a documented unsupported category.\n")
	} else {
		for _, c := range t.UnexpectedCases {
			fmt.Fprintf(&b, "### `%s`\n\n```\n%s\n```\n\n", c.Name, firstError(c.Output))
		}
	}

	fmt.Fprintf(&b, "\n## Case detail\n\n")
	fmt.Fprintf(&b, "| Result | Category | Case |\n")
	fmt.Fprintf(&b, "| --- | --- | --- |\n")
	for _, c := range cases {
		result := c.Action
		if c.Action == "fail" && c.CategoryID != "" {
			result = "expected unsupported"
		}
		fmt.Fprintf(&b, "| %s | %s | `%s` |\n", result, c.CategoryID, c.Name)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func firstError(output string) string {
	lines := strings.Split(output, "\n")
	var out []string
	capture := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "Error:"):
			capture = true
			out = append(out, trimmed)
		case capture && strings.HasPrefix(trimmed, "Test:"):
			capture = false
		case capture && trimmed != "":
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return strings.TrimSpace(output)
	}
	return strings.Join(out, "\n")
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}
