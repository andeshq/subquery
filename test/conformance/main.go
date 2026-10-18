// Command conformance replays HTTP requests against a server and diffs two
// recordings of the same request set.
//
// The method mirrors PostgREST's own conformance approach: load an identical
// fixture database for each server, replay the same requests against both, and
// compare the live responses at increasing strictness levels. Stock PostgREST is
// the oracle; no expectation is ever interpreted.
//
// Usage:
//
//	conformance replay  -base http://localhost:3000 -cases cases -out ref.json
//	conformance compare -ref ref.json -target target.json
package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Case is one request to replay.
type Case struct {
	Group   string            `json:"group"`
	Name    string            `json:"name"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	// Comment records where the example came from.
	Comment string `json:"comment,omitempty"`
	// Known marks a deliberate divergence: the reason is recorded so nobody
	// "fixes" it by reintroducing the reference's behavior.
	Known string `json:"known_divergence,omitempty"`
}

// Result is one recorded response.
type Result struct {
	Group   string            `json:"group"`
	Name    string            `json:"name"`
	Status  int               `json:"status"`
	Body    string            `json:"body"`
	Headers map[string]string `json:"headers"`
	Error   string            `json:"error,omitempty"`
	Known   string            `json:"known_divergence,omitempty"`
}

// comparedHeaders are the headers the contract is measured on. Date, Server,
// and Connection differ between any two servers and say nothing.
var comparedHeaders = []string{
	"Content-Type",
	"Content-Range",
	"Location",
	"Preference-Applied",
	"Allow",
	"WWW-Authenticate",
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "replay":
		replay(os.Args[2:])
	case "compare":
		compare(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  conformance replay  -base URL -cases DIR -out FILE
  conformance compare -ref FILE -target FILE`)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// replay sends every case to -base and records the responses.
func replay(args []string) {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	base := fs.String("base", "http://localhost:3000", "base URL of the server to record")
	casesDir := fs.String("cases", "cases", "directory of case files")
	outPath := fs.String("out", "", "results file to write")
	verbose := fs.Bool("v", false, "log each request")
	_ = fs.Parse(args)
	if *outPath == "" {
		fatalf("replay: -out is required")
	}

	cases, err := loadCases(*casesDir)
	if err != nil {
		fatalf("load cases: %v", err)
	}
	client := &http.Client{Timeout: 30 * time.Second}

	results := make([]Result, 0, len(cases))
	for _, testCase := range cases {
		result := send(client, *base, testCase)
		results = append(results, result)
		if *verbose {
			fmt.Printf("%-8s %-3d %s\n", testCase.Method, result.Status, testCase.Path)
		}
	}
	if err := writeJSON(*outPath, results); err != nil {
		fatalf("write results: %v", err)
	}
	fmt.Printf("recorded %d responses from %s into %s\n", len(results), *base, *outPath)
}

func send(client *http.Client, base string, testCase Case) Result {
	result := Result{Group: testCase.Group, Name: testCase.Name, Known: testCase.Known, Headers: map[string]string{}}

	method := testCase.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if testCase.Body != "" {
		body = strings.NewReader(testCase.Body)
	}
	request, err := http.NewRequest(method, strings.TrimRight(base, "/")+testCase.Path, body)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if testCase.Body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range testCase.Headers {
		request.Header.Set(name, value)
	}

	response, err := client.Do(request)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Status = response.StatusCode
	result.Body = string(payload)
	for _, name := range comparedHeaders {
		if value := response.Header.Get(name); value != "" {
			result.Headers[name] = value
		}
	}
	return result
}

// verdict is how closely a target response matches the reference.
type verdict struct {
	status  bool
	body    bool
	headers bool
	full    bool
}

// compare diffs two recordings and prints a report.
func compare(args []string) {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	referencePath := fs.String("ref", "", "reference recording (the oracle)")
	targetPath := fs.String("target", "", "target recording")
	showDiff := fs.Int("show", 12, "how many divergences to list")
	_ = fs.Parse(args)
	if *referencePath == "" || *targetPath == "" {
		fatalf("compare: -ref and -target are required")
	}

	reference, err := readJSON[[]Result](*referencePath)
	if err != nil {
		fatalf("read reference: %v", err)
	}
	target, err := readJSON[[]Result](*targetPath)
	if err != nil {
		fatalf("read target: %v", err)
	}

	byKey := make(map[string]Result, len(reference))
	for _, item := range reference {
		byKey[item.Group+"\x00"+item.Name] = item
	}

	type groupTally struct{ cases, status, body, headers, full int }
	tallies := map[string]*groupTally{}
	var divergences, deliberate []string

	for _, got := range target {
		want, ok := byKey[got.Group+"\x00"+got.Name]
		if !ok {
			delete(byKey, got.Group+"\x00"+got.Name)
			continue
		}
		tally := tallies[got.Group]
		if tally == nil {
			tally = &groupTally{}
			tallies[got.Group] = tally
		}
		if got.Known != "" {
			deliberate = append(deliberate, want.Group+"/"+want.Name+": "+got.Known)
			continue
		}
		tally.cases++
		verdict := judge(want, got)
		if verdict.status {
			tally.status++
		}
		if verdict.status && verdict.body {
			tally.body++
		}
		if verdict.status && verdict.body && verdict.headers {
			tally.headers++
		}
		if verdict.status && verdict.body && verdict.headers && verdict.full {
			tally.full++
		}
		if !verdict.full {
			divergences = append(divergences, describe(want, got, verdict))
		}
	}
	if len(divergences) == 0 && len(tallies) == 0 {
		fmt.Println("no matching cases: do the two recordings come from the same case set?")
		return
	}

	names := make([]string, 0, len(tallies))
	for name := range tallies {
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Printf("\n%-28s %5s %7s %7s %9s %6s\n", "group", "cases", "status", "+body", "+headers", "full")
	fmt.Println(strings.Repeat("-", 68))
	total := &groupTally{}
	for _, name := range names {
		tally := tallies[name]
		fmt.Printf("%-28s %5d %6d%% %6d%% %8d%% %5d%%\n", name, tally.cases,
			pct(tally.status, tally.cases), pct(tally.body, tally.cases),
			pct(tally.headers, tally.cases), pct(tally.full, tally.cases))
		total.cases += tally.cases
		total.status += tally.status
		total.body += tally.body
		total.headers += tally.headers
		total.full += tally.full
	}
	fmt.Println(strings.Repeat("-", 68))
	fmt.Printf("%-28s %5d %6d%% %6d%% %8d%% %5d%%\n", "total", total.cases,
		pct(total.status, total.cases), pct(total.body, total.cases),
		pct(total.headers, total.cases), pct(total.full, total.cases))

	if len(deliberate) > 0 {
		fmt.Printf("\n%d deliberate divergences:\n", len(deliberate))
		for _, line := range deliberate {
			fmt.Println("  " + line)
		}
	}

	if len(divergences) > 0 {
		fmt.Printf("\n%d divergences", len(divergences))
		if *showDiff > 0 && len(divergences) > *showDiff {
			fmt.Printf(" (showing %d)", *showDiff)
		}
		fmt.Println(":")
		for i, line := range divergences {
			if *showDiff > 0 && i >= *showDiff {
				break
			}
			fmt.Println("  " + line)
		}
	}
}

func pct(part, whole int) int {
	if whole == 0 {
		return 0
	}
	return part * 100 / whole
}

// judge compares one target response against the reference.
func judge(want, got Result) verdict {
	if want.Error != "" || got.Error != "" {
		same := want.Error == got.Error
		return verdict{status: same, body: same, headers: same, full: same}
	}
	v := verdict{status: want.Status == got.Status}
	v.body = v.status && bodiesEqual(want.Body, got.Body)

	v.headers = v.body
	for _, name := range comparedHeaders {
		if name == "Content-Range" {
			continue
		}
		if !strings.EqualFold(want.Headers[name], got.Headers[name]) {
			v.headers = false
			break
		}
	}
	// The full contract adds Content-Range.
	v.full = v.headers && strings.EqualFold(want.Headers["Content-Range"], got.Headers["Content-Range"])
	return v
}

// timestamp matches ISO-8601 instants, which differ between two runs because
// columns default to now().
var timestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})?`)

// bodiesEqual compares JSON structurally, so object key order does not matter.
// Timestamps are normalized because two runs cannot share a clock. For error
// payloads only the code is compared: the code is the contract, the message is
// prose.
func bodiesEqual(a, b string) bool {
	if a == b {
		return true
	}
	var left, right any
	if json.Unmarshal([]byte(timestamp.ReplaceAllString(a, "<ts>")), &left) != nil ||
		json.Unmarshal([]byte(timestamp.ReplaceAllString(b, "<ts>")), &right) != nil {
		return false
	}
	if l, ok := left.(map[string]any); ok {
		if r, ok := right.(map[string]any); ok {
			leftCode, hasLeft := l["code"]
			rightCode, hasRight := r["code"]
			if hasLeft && hasRight {
				return leftCode == rightCode
			}
		}
	}
	return reflect.DeepEqual(left, right)
}

func describe(want, got Result, v verdict) string {
	var why []string
	if !v.status {
		why = append(why, fmt.Sprintf("status want %d got %d", want.Status, got.Status))
	} else if !v.body {
		why = append(why, fmt.Sprintf("body want %.90s got %.90s", compact(want.Body), compact(got.Body)))
	} else if !v.headers || !v.full {
		var diff []string
		for _, name := range comparedHeaders {
			if !strings.EqualFold(want.Headers[name], got.Headers[name]) {
				diff = append(diff, fmt.Sprintf("%s want %q got %q", name, want.Headers[name], got.Headers[name]))
			}
		}
		why = append(why, strings.Join(diff, "; "))
	}
	return fmt.Sprintf("%s/%s: %s", want.Group, want.Name, strings.Join(why, " | "))
}

func compact(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func loadCases(dir string) ([]Case, error) {
	entries, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no case files in %s", dir)
	}
	sort.Strings(entries)
	var all []Case
	for _, path := range entries {
		cases, err := readJSON[[]Case](path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		all = append(all, cases...)
	}
	return all, nil
}

func readJSON[T any](path string) (T, error) {
	var value T
	payload, err := os.ReadFile(path)
	if err != nil {
		return value, err
	}
	if err := json.Unmarshal(payload, &value); err != nil {
		return value, err
	}
	return value, nil
}

func writeJSON(path string, value any) error {
	var buffer bytes.Buffer
	encoder := jsontext.NewEncoder(&buffer, jsontext.WithIndent("  "))
	if err := json.MarshalEncode(encoder, value); err != nil {
		return err
	}
	buffer.WriteByte('\n')
	return os.WriteFile(path, buffer.Bytes(), 0o644)
}
