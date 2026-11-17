// Command evals measures how well an agent answers questions about a Matrix account through this MCP.
//
// It runs each task through Claude Code in headless mode (`claude -p`), connected only to the
// matrix MCP and limited to read-only tools, then grades the final "ANSWER:" line against the
// expected answers. Tasks are JSON files; keep tasks about your own chats in *.local.json (gitignored).
//
//	MATRIX_MCP_URL=https://matrix-mcp.example.com/mcp MATRIX_MCP_TOKEN=... \
//	  go run ./evals -tasks evals/tasks.local.json -trials 3
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

type Task struct {
	ID       string   `json:"id"`
	Question string   `json:"question"`
	Answers  []string `json:"answers"`         // accepted answers; a trial passes if any one matches
	Match    string   `json:"match,omitempty"` // "contains" (default), "exact" or "regex"
	Notes    string   `json:"notes,omitempty"`
}

type Trial struct {
	Task     string   `json:"task"`
	Trial    int      `json:"trial"`
	Pass     bool     `json:"pass"`
	Answer   string   `json:"answer"`
	Result   string   `json:"result,omitempty"`
	Turns    int      `json:"turns"`
	CostUSD  float64  `json:"cost_usd"`
	Seconds  float64  `json:"seconds"`
	Error    string   `json:"error,omitempty"`
	Expected []string `json:"expected"`
}

var readOnlyTools = []string{"status", "list_rooms", "get_room", "read_messages", "get_message_context", "search_messages", "backfill_room", "download_media"}

var writeTools = []string{"send_message", "send_file", "send_reaction", "edit_message", "delete_message", "mark_read", "join_room", "leave_room",
	"invite_user", "remove_user", "create_room", "update_room", "bridge_command", "wait_for_messages", "matrix_api"}

const systemPrompt = `You are being evaluated on answering questions about the user's chats using the matrix tools.
Do not ask the user anything; find the answer yourself. Be efficient with tool calls.
Finish with a final line in exactly this form:
ANSWER: <short answer>`

func main() {
	var (
		taskFiles = flag.String("tasks", "evals/tasks.local.json", "comma-separated task files")
		only      = flag.String("task", "", "run only the task with this id")
		trials    = flag.Int("trials", 1, "trials per task; a task passes only if every trial passes (pass^k)")
		model     = flag.String("model", "", "model for claude -p (default: Claude Code's default)")
		parallel  = flag.Int("parallel", 3, "tasks run concurrently")
		timeout   = flag.Duration("timeout", 5*time.Minute, "timeout per trial")
		report    = flag.String("json", "", "write a per-trial JSON report to this file")
		verbose   = flag.Bool("v", false, "print the full model output of failed trials")
	)
	flag.Parse()
	url, token := os.Getenv("MATRIX_MCP_URL"), os.Getenv("MATRIX_MCP_TOKEN")
	if url == "" || token == "" {
		fatal("set MATRIX_MCP_URL and MATRIX_MCP_TOKEN")
	}
	var tasks []Task
	for _, f := range strings.Split(*taskFiles, ",") {
		data, err := os.ReadFile(strings.TrimSpace(f))
		if err != nil {
			fatal(err.Error())
		}
		var ts []Task
		if err := json.Unmarshal(data, &ts); err != nil {
			fatal(fmt.Sprintf("%s: %v", f, err))
		}
		for _, t := range ts {
			if *only == "" || t.ID == *only {
				tasks = append(tasks, t)
			}
		}
	}
	if len(tasks) == 0 {
		fatal("no tasks")
	}

	// Isolated working directory with an MCP config that only contains the matrix server.
	work, err := os.MkdirTemp("", "matrix-mcp-evals-")
	if err != nil {
		fatal(err.Error())
	}
	defer os.RemoveAll(work)
	cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"matrix": map[string]any{
		"type": "http", "url": url, "headers": map[string]string{"Authorization": "Bearer " + token},
	}}})
	cfgPath := filepath.Join(work, "mcp.json")
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		fatal(err.Error())
	}

	type job struct {
		task  Task
		trial int
	}
	jobs := make(chan job)
	results := make(chan Trial)
	var wg sync.WaitGroup
	for i := 0; i < max(1, *parallel); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				results <- runTrial(j.task, j.trial, work, cfgPath, *model, *timeout)
			}
		}()
	}
	go func() {
		for _, t := range tasks {
			for k := 1; k <= *trials; k++ {
				jobs <- job{t, k}
			}
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	byTask := map[string][]Trial{}
	var all []Trial
	for r := range results {
		mark := "PASS"
		if !r.Pass {
			mark = "FAIL"
		}
		fmt.Printf("%s %-28s #%d  %2d turns  %5.1fs  $%.3f  answer=%q", mark, r.Task, r.Trial, r.Turns, r.Seconds, r.CostUSD, r.Answer)
		if !r.Pass {
			fmt.Printf("  expected=%q", r.Expected)
		}
		if r.Error != "" {
			fmt.Printf("  error=%s", r.Error)
		}
		fmt.Println()
		if *verbose && !r.Pass && r.Result != "" {
			fmt.Println(indent(r.Result))
		}
		byTask[r.Task] = append(byTask[r.Task], r)
		all = append(all, r)
	}

	passed, cost := 0, 0.0
	for _, t := range tasks {
		ok := true
		for _, r := range byTask[t.ID] {
			ok = ok && r.Pass
			cost += r.CostUSD
		}
		if ok {
			passed++
		}
	}
	fmt.Printf("\n%d/%d tasks passed (pass^%d), total cost $%.2f\n", passed, len(tasks), *trials, cost)
	if *report != "" {
		data, _ := json.MarshalIndent(all, "", "  ")
		if err := os.WriteFile(*report, data, 0o600); err != nil {
			fatal(err.Error())
		}
	}
	if passed != len(tasks) {
		os.Exit(1)
	}
}

func runTrial(t Task, n int, work, cfgPath, model string, timeout time.Duration) Trial {
	tr := Trial{Task: t.ID, Trial: n, Expected: t.Answers}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	allowed := make([]string, len(readOnlyTools))
	for i, name := range readOnlyTools {
		allowed[i] = "mcp__matrix__" + name
	}
	denied := make([]string, len(writeTools))
	for i, name := range writeTools {
		denied[i] = "mcp__matrix__" + name
	}
	args := []string{"-p", t.Question, "--output-format", "json", "--no-session-persistence",
		"--strict-mcp-config", "--mcp-config", cfgPath, "--setting-sources", "project",
		"--tools", "", "--allowedTools", strings.Join(allowed, ","), "--disallowedTools", strings.Join(denied, ","),
		"--append-system-prompt", systemPrompt}
	if model != "" {
		args = append(args, "--model", model)
	}
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = work
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	tr.Seconds = time.Since(start).Seconds()
	var out struct {
		Result  string  `json:"result"`
		Turns   int     `json:"num_turns"`
		Cost    float64 `json:"total_cost_usd"`
		IsError bool    `json:"is_error"`
	}
	if jerr := json.Unmarshal(stdout.Bytes(), &out); jerr != nil {
		tr.Error = strings.TrimSpace(firstLine(stderr.String() + " " + fmt.Sprint(err)))
		return tr
	}
	tr.Result, tr.Turns, tr.CostUSD = out.Result, out.Turns, out.Cost
	if out.IsError {
		tr.Error = "claude reported an error"
	}
	tr.Answer = finalAnswer(out.Result)
	tr.Pass = grade(t, tr.Answer)
	return tr
}

var answerLine = regexp.MustCompile(`(?im)^\**ANSWER:?\**:?\s*(.+)$`)

func finalAnswer(result string) string {
	m := answerLine.FindAllStringSubmatch(result, -1)
	if len(m) == 0 {
		return strings.TrimSpace(lastLine(result))
	}
	return strings.Trim(strings.TrimSpace(m[len(m)-1][1]), "*` ")
}

func grade(t Task, answer string) bool {
	got := normalize(answer)
	for _, want := range t.Answers {
		switch t.Match {
		case "regex":
			if re, err := regexp.Compile("(?i)" + want); err == nil && re.MatchString(answer) {
				return true
			}
		case "exact":
			if got == normalize(want) {
				return true
			}
		default:
			if w := normalize(want); w != "" && strings.Contains(got, w) {
				return true
			}
		}
	}
	return false
}

var foldExtra = strings.NewReplacer("ł", "l", "Ł", "L")

// normalize lowercases, strips diacritics and punctuation, and collapses whitespace.
func normalize(s string) string {
	s = foldExtra.Replace(s)
	s, _, _ = transform.String(transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC), s)
	s = strings.ToLower(s)
	s = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ':' || r == '/' || r == '.' {
			return r
		}
		return ' '
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n    ")
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "evals:", msg)
	os.Exit(2)
}
