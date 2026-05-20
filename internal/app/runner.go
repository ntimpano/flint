package app

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"flint/internal/model"
	"flint/internal/parity"
)

type summaryNote struct {
	ID        int    `json:"id"`
	CreatedAt string `json:"created_at"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Content   string `json:"content"`
}

type summaryMemory struct {
	SchemaVersion int  `json:"schema_version"`
	FTSHealthy    bool `json:"fts_healthy"`
	IntegrityOK   bool `json:"integrity_ok"`
	ItemsCount    int  `json:"items_count"`
}

type summaryProject struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	RootPath string `json:"root_path"`
}

type summaryView struct {
	Project     *summaryProject `json:"project"`
	Memory      *summaryMemory  `json:"memory"`
	RecentNotes []summaryNote   `json:"recent_notes"`
	Topics      []string        `json:"topics"`
}

// RunCLIWithStdin is the full-featured entry point that also accepts a stdin
// reader so the autoswitch policy can be injected for testing. In production,
// main.go calls this with os.Stdin. RunCLI delegates here with a nil stdin
// (which falls back to os.Stdin inside the policy).
func RunCLIWithStdin(svc *Service, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) >= 1 && IsMemoryCommand(args[0]) && svc != nil {
		policy := NewDefaultAutoswitchPolicy(stdin, stderr)
		ApplyAutoswitch(svc, policy)
	}
	return runCLIWithInput(svc, args, stdin, stdout, stderr)
}

// RunCLI dispatches a flint command using the provided Service and writes
// human output to stdout / errors to stderr. It returns the process exit code
// (0 on success, non-zero on validation, not-found, or unknown command).
//
// This function is the testable seam used in place of an inline `func main`
// switch so that strict TDD can cover dispatch, validation, exit codes, and
// CLI/MCP behaviour parity end-to-end.
//
// The caller is responsible for constructing the Service (and the underlying
// store / DB), so RunCLI itself never touches the filesystem.
func RunCLI(svc *Service, args []string, stdout, stderr io.Writer) int {
	return runCLIWithInput(svc, args, os.Stdin, stdout, stderr)
}

// RunInitOrProfile routes init execution to either the new v2 init runner
// (RunInit) or the legacy profile-only runner (RunInitProfile).
//
// It accepts either a pure init-args slice (e.g. []string{"--legacy"}) or a
// full command slice that still includes the leading "init"
// (e.g. os.Args[1:]).
func RunInitOrProfile(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	initArgs := args
	if len(initArgs) > 0 && strings.TrimSpace(initArgs[0]) == "init" {
		initArgs = initArgs[1:]
	}
	if hasFlag(initArgs, "--legacy") || hasFlag(initArgs, "--profile") {
		return RunInitProfile(initArgs, stdin, stdout, stderr)
	}
	return RunInit(initArgs, stdin, stdout, stderr)
}

func runCLIWithInput(svc *Service, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		printUsage(stdout)
		return 1
	}

	switch args[0] {
	case "init":
		if err := svc.Init(); err != nil {
			fmt.Fprintf(stderr, "init failed: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "initialized")
		return RunInitOrProfile(args[1:], stdin, stdout, stderr)

	case "save":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "usage: flint save [--title=...] [--type=...] [--topic-key=...] [--scope=...] \"your note\"")
			return 1
		}
		// Parse leading `--key=value` flags, stop at the first non-flag arg
		// so callers can save content that itself contains `--`-prefixed
		// words (e.g. `nt-cli save --type=decision "use --foo flag"`).
		var meta SaveRequest
		hasMeta := false
		i := 1
		for ; i < len(args); i++ {
			a := args[i]
			if !strings.HasPrefix(a, "--") {
				break
			}
			key, val, ok := strings.Cut(strings.TrimPrefix(a, "--"), "=")
			if !ok {
				fmt.Fprintf(stderr, "invalid flag %q (expected --key=value)\n", a)
				return 1
			}
			switch key {
			case "title":
				meta.Title = val
			case "type":
				meta.Type = val
			case "topic-key":
				meta.TopicKey = val
			case "scope":
				meta.Scope = val
			default:
				fmt.Fprintf(stderr, "unknown flag --%s\n", key)
				return 1
			}
			hasMeta = true
		}
		if i >= len(args) {
			fmt.Fprintln(stderr, "usage: flint save [flags] \"your note\"")
			return 1
		}
		note := strings.TrimSpace(strings.Join(args[i:], " "))
		if note == "" {
			fmt.Fprintln(stderr, "note cannot be empty")
			return 1
		}
		var (
			id  int64
			err error
		)
		if hasMeta {
			meta.Content = note
			id, err = svc.SaveWithMeta(meta)
		} else {
			id, err = svc.Save(note)
		}
		if err != nil {
			fmt.Fprintf(stderr, "save failed: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "saved #%d\n", id)
		return 0

	case "recall":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "usage: flint recall [--type=...] [--since=YYYY-MM-DD] [--until=YYYY-MM-DD] \"query\"")
			return 1
		}
		// Parse leading `--key=value` filter flags. Stop at the first
		// non-flag arg so the query itself can contain `--`-prefixed
		// words (mirrors the `save` parser).
		var (
			filterType string
			since      time.Time
			until      time.Time
			hasFilter  bool
		)
		i := 1
		for ; i < len(args); i++ {
			a := args[i]
			if !strings.HasPrefix(a, "--") {
				break
			}
			key, val, ok := strings.Cut(strings.TrimPrefix(a, "--"), "=")
			if !ok {
				fmt.Fprintf(stderr, "invalid flag %q (expected --key=value)\n", a)
				return 1
			}
			switch key {
			case "type":
				filterType = val
				hasFilter = true
			case "since":
				t, err := parseDateFlag(val)
				if err != nil {
					fmt.Fprintf(stderr, "invalid --since: %v\n", err)
					return 1
				}
				since = t
				hasFilter = true
			case "until":
				t, err := parseDateFlag(val)
				if err != nil {
					fmt.Fprintf(stderr, "invalid --until: %v\n", err)
					return 1
				}
				until = t
				hasFilter = true
			default:
				fmt.Fprintf(stderr, "unknown flag --%s\n", key)
				return 1
			}
		}
		if i >= len(args) {
			fmt.Fprintln(stderr, "usage: flint recall [flags] \"query\"")
			return 1
		}
		query := strings.TrimSpace(strings.Join(args[i:], " "))
		if query == "" {
			fmt.Fprintln(stderr, "query cannot be empty")
			return 1
		}
		var (
			items []MemoryItem
			err   error
		)
		if hasFilter {
			items, err = svc.RecallWithOptions(RecallOptions{
				Query: query,
				Type:  filterType,
				Since: since,
				Until: until,
				Limit: 10,
			})
		} else {
			items, err = svc.Recall(query, 10)
		}
		if err != nil {
			fmt.Fprintf(stderr, "recall failed: %v\n", err)
			return 1
		}
		if len(items) == 0 {
			fmt.Fprintln(stdout, "no results")
			return 0
		}
		for _, it := range items {
			fmt.Fprintf(stdout, "#%d [%s] %s\n", it.ID, it.CreatedAt.Format("2006-01-02 15:04"), it.Content)
		}
		return 0

	case "context":
		// `context` exposes the most-recent-N view, optionally narrowed
		// by scope. Defaults: n=10, no scope filter.
		n := 10
		scope := ""
		summaryMode := false
		jsonMode := false
		for i := 1; i < len(args); i++ {
			a := args[i]
			if !strings.HasPrefix(a, "--") {
				fmt.Fprintf(stderr, "unexpected positional arg %q (context only takes flags)\n", a)
				return 1
			}
			if a == "--summary" {
				summaryMode = true
				continue
			}
			if a == "--json" {
				jsonMode = true
				continue
			}
			key, val, ok := strings.Cut(strings.TrimPrefix(a, "--"), "=")
			if !ok {
				// Legacy context parser is strict for `--key=value` flags, but
				// summary/json are accepted as bare booleans above. Any other bare
				// flag is ignored to keep this path permissive for future toggles.
				continue
			}
			switch key {
			case "n":
				parsed, err := strconv.Atoi(strings.TrimSpace(val))
				if err != nil || parsed <= 0 {
					fmt.Fprintln(stderr, "--n must be a positive integer")
					return 1
				}
				n = parsed
			case "scope":
				scope = val
			case "summary":
				parsed, err := strconv.ParseBool(strings.TrimSpace(val))
				if err != nil {
					fmt.Fprintf(stderr, "invalid --summary: %q (expected true or false)\n", val)
					return 1
				}
				summaryMode = parsed
			case "json":
				parsed, err := strconv.ParseBool(strings.TrimSpace(val))
				if err != nil {
					fmt.Fprintf(stderr, "invalid --json: %q (expected true or false)\n", val)
					return 1
				}
				jsonMode = parsed
			default:
				fmt.Fprintf(stderr, "unknown flag --%s\n", key)
				return 1
			}
		}
		if summaryMode {
			if err := runContextSummary(svc, stdout, jsonMode); err != nil {
				fmt.Fprintf(stderr, "context failed: %v\n", err)
				return 1
			}
			return 0
		}
		items, err := svc.Context(n, scope)
		if err != nil {
			fmt.Fprintf(stderr, "context failed: %v\n", err)
			return 1
		}
		if len(items) == 0 {
			fmt.Fprintln(stdout, "no results")
			return 0
		}
		for _, it := range items {
			fmt.Fprintf(stdout, "#%d [%s] %s\n", it.ID, it.CreatedAt.Format("2006-01-02 15:04"), it.Content)
		}
		return 0

	case "list":
		limit := 10
		allProjects := false
		i := 1
		for ; i < len(args); i++ {
			a := args[i]
			if strings.HasPrefix(a, "--") {
				key, _, _ := strings.Cut(strings.TrimPrefix(a, "--"), "=")
				switch key {
				case "all-projects":
					allProjects = true
				default:
					fmt.Fprintf(stderr, "unknown flag --%s\n", key)
					return 1
				}
			} else {
				n, err := strconv.Atoi(strings.TrimSpace(a))
				if err != nil || n <= 0 {
					fmt.Fprintln(stderr, "usage: flint list [positive-limit] [--all-projects]")
					return 1
				}
				limit = n
			}
		}
		items, err := svc.ListOpts(limit, allProjects)
		if err != nil {
			fmt.Fprintf(stderr, "list failed: %v\n", err)
			return 1
		}
		if len(items) == 0 {
			fmt.Fprintln(stdout, "no results")
			return 0
		}
		for _, it := range items {
			fmt.Fprintf(stdout, "#%d [%s] %s\n", it.ID, it.CreatedAt.Format("2006-01-02 15:04"), it.Content)
		}
		return 0

	case "delete":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "usage: flint delete <id>")
			return 1
		}
		id, err := strconv.ParseInt(strings.TrimSpace(args[1]), 10, 64)
		if err != nil || id <= 0 {
			fmt.Fprintln(stderr, "id must be a positive integer")
			return 1
		}
		deleted, err := svc.Delete(id)
		if err != nil {
			fmt.Fprintf(stderr, "delete failed: %v\n", err)
			return 1
		}
		if !deleted {
			fmt.Fprintf(stdout, "note #%d not found\n", id)
			return 0
		}
		fmt.Fprintf(stdout, "deleted #%d\n", id)
		return 0

	case "get":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "usage: flint get <id>")
			return 1
		}
		id, err := ParsePositiveID(args[1])
		if err != nil {
			fmt.Fprintln(stderr, err.Error())
			return 1
		}
		item, err := svc.Get(id)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				fmt.Fprintf(stderr, "note #%d not found\n", id)
				return 1
			}
			fmt.Fprintf(stderr, "get failed: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, FormatNote(item))
		return 0

	case "update":
		if len(args) < 3 {
			fmt.Fprintln(stderr, "usage: flint update <id> \"new content\"")
			return 1
		}
		id, err := ParsePositiveID(args[1])
		if err != nil {
			fmt.Fprintln(stderr, err.Error())
			return 1
		}
		content := strings.TrimSpace(strings.Join(args[2:], " "))
		if content == "" {
			fmt.Fprintln(stderr, "content cannot be empty")
			return 1
		}
		ok, err := svc.Update(id, content)
		if err != nil {
			fmt.Fprintf(stderr, "update failed: %v\n", err)
			return 1
		}
		if !ok {
			fmt.Fprintf(stderr, "note #%d not found\n", id)
			return 1
		}
		fmt.Fprintf(stdout, "updated #%d\n", id)
		return 0

	case "session":
		return runSession(svc, args[1:], stdout, stderr)

	case "behavior":
		return runBehavior(svc, args[1:], stdout, stderr)

	case "import":
		return runImport(svc, args[1:], stdout, stderr)

	case "backup":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "usage: flint backup <path>")
			return 1
		}
		if err := svc.Backup(args[1]); err != nil {
			fmt.Fprintf(stderr, "backup failed: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "backup written to %s\n", args[1])
		return 0

	case "restore":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "usage: flint restore <path>")
			return 1
		}
		if err := svc.Restore(args[1]); err != nil {
			fmt.Fprintf(stderr, "restore failed: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "restored from %s\n", args[1])
		return 0

	case "doctor":
		// Doctor takes no arguments. Reject extras so typos like
		// `flint doctor --json` surface instead of silently ignoring.
		if len(args) > 1 {
			fmt.Fprintln(stderr, "usage: flint doctor")
			return 1
		}
		report, err := svc.Doctor()
		if err != nil {
			fmt.Fprintf(stderr, "doctor failed: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, report.Summary)
		// Surface non-ok integrity messages verbatim so users can act
		// on them. Healthy stores produce zero extra lines.
		for _, msg := range report.IntegrityMessages {
			fmt.Fprintf(stdout, "  integrity: %s\n", msg)
		}
		return 0

	case "parity":
		return runParity(svc, args, stdout, stderr)

	case "project":
		return runProject(svc, args[1:], stdout, stderr)

	default:
		printUsage(stdout)
		return 1
	}
}

func runContextSummary(svc *Service, w io.Writer, jsonMode bool) error {
	v := summaryView{
		RecentNotes: []summaryNote{},
		Topics:      []string{},
	}

	if svc != nil && svc.ProjectEng != nil {
		p, err := svc.ProjectEng.Current()
		if err == nil {
			v.Project = &summaryProject{
				ID:       int(p.ID),
				Name:     p.Name,
				RootPath: p.RootPath,
			}
		} else if !errors.Is(err, ErrNoActiveProject) {
			return err
		}
	}

	if svc != nil {
		report, err := svc.Doctor()
		if err == nil {
			v.Memory = &summaryMemory{
				SchemaVersion: report.SchemaVersion,
				FTSHealthy:    report.FTSHealthy,
				IntegrityOK:   report.IntegrityOK,
				ItemsCount:    report.MemoryItemsCount,
			}
		}
	}

	notes, err := svc.Context(5, "")
	if err != nil {
		return err
	}
	seenTopics := map[string]struct{}{}
	for _, it := range notes {
		v.RecentNotes = append(v.RecentNotes, summaryNote{
			ID:        int(it.ID),
			CreatedAt: it.CreatedAt.Format("2006-01-02 15:04"),
			Type:      it.Type,
			Title:     it.Title,
			Content:   it.Content,
		})
		topic := strings.TrimSpace(it.TopicKey)
		if topic == "" {
			continue
		}
		if _, ok := seenTopics[topic]; ok {
			continue
		}
		seenTopics[topic] = struct{}{}
		v.Topics = append(v.Topics, topic)
	}

	if jsonMode {
		return printSummaryJSON(w, v)
	}
	printSummaryHuman(w, v)
	return nil
}

func printSummaryHuman(w io.Writer, v summaryView) {
	fmt.Fprintln(w, "PROJECT")
	if v.Project == nil {
		fmt.Fprintln(w, "  none")
	} else {
		fmt.Fprintf(w, "  %s (%s)\n", v.Project.Name, v.Project.RootPath)
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "MEMORY HEALTH")
	if v.Memory == nil {
		fmt.Fprintln(w, "  none")
	} else {
		fmt.Fprintf(w, "  schema_version: %d\n", v.Memory.SchemaVersion)
		fmt.Fprintf(w, "  fts: %s\n", summaryOK(v.Memory.FTSHealthy))
		fmt.Fprintf(w, "  integrity: %s\n", summaryOK(v.Memory.IntegrityOK))
		fmt.Fprintf(w, "  items: %d\n", v.Memory.ItemsCount)
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "RECENT NOTES (last 5)")
	if len(v.RecentNotes) == 0 {
		fmt.Fprintln(w, "  no notes found")
	} else {
		for _, n := range v.RecentNotes {
			fmt.Fprintf(w, "  [%d] %s  %s\n", n.ID, n.CreatedAt, n.Content)
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "TOPICS")
	if len(v.Topics) == 0 {
		fmt.Fprint(w, "  none")
		return
	}
	for i, topic := range v.Topics {
		if i == len(v.Topics)-1 {
			fmt.Fprint(w, "  "+topic)
			continue
		}
		fmt.Fprintln(w, "  "+topic)
	}
}

func printSummaryJSON(w io.Writer, v summaryView) error {
	return json.NewEncoder(w).Encode(v)
}

func summaryOK(ok bool) string {
	if ok {
		return "ok"
	}
	return "FAIL"
}

// runSession dispatches `flint session <start|end|summary> <id> [text...]`.
// Kept as a helper so the main switch stays scannable. Validation lives at
// the service layer; the CLI is just an args parser + presenter.
// runImport dispatches `flint import [--dry-run] <file.json>`. Currently
// JSON-only; format is detected by extension when MD/CSV are added.
func runImport(svc *Service, args []string, stdout, stderr io.Writer) int {
	dryRun := false
	var path string
	for _, a := range args {
		if strings.HasPrefix(a, "--") {
			key, val, hasVal := strings.Cut(strings.TrimPrefix(a, "--"), "=")
			switch key {
			case "dry-run":
				if hasVal && strings.ToLower(strings.TrimSpace(val)) == "false" {
					dryRun = false
				} else {
					dryRun = true
				}
			default:
				fmt.Fprintf(stderr, "unknown flag --%s\n", key)
				return 1
			}
			continue
		}
		if path != "" {
			fmt.Fprintln(stderr, "import takes a single file path")
			return 1
		}
		path = a
	}
	if strings.TrimSpace(path) == "" {
		fmt.Fprintln(stderr, "usage: flint import [--dry-run] <file.json>")
		return 1
	}
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(stderr, "import read failed: %v\n", err)
		return 1
	}
	res, err := svc.ImportJSON(data, dryRun)
	if err != nil {
		fmt.Fprintf(stderr, "import failed: %v\n", err)
		return 1
	}
	prefix := "import"
	if dryRun {
		prefix = "import (dry-run)"
	}
	fmt.Fprintf(stdout, "%s: inserted=%d skipped=%d\n", prefix, res.Inserted, res.Skipped)
	return 0
}

func runSession(svc *Service, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "usage: flint session <start|end|summary> <id> [text]")
		return 1
	}
	switch args[0] {
	case "start":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "usage: flint session start <id>")
			return 1
		}
		id := strings.TrimSpace(args[1])
		if err := svc.SessionStart(id); err != nil {
			fmt.Fprintf(stderr, "session start failed: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "session started %s\n", id)
		return 0
	case "end":
		if len(args) > 3 {
			fmt.Fprintln(stderr, "usage: flint session end [id] [--summary=\"text\"]")
			return 1
		}
		id := ""
		summary := ""
		for i := 1; i < len(args); i++ {
			a := args[i]
			if strings.HasPrefix(a, "--summary=") {
				summary = strings.TrimSpace(strings.TrimPrefix(a, "--summary="))
				continue
			}
			if a == "--summary" {
				if i+1 >= len(args) {
					fmt.Fprintln(stderr, "usage: flint session end [id] [--summary=\"text\"]")
					return 1
				}
				i++
				summary = strings.TrimSpace(args[i])
				continue
			}
			if strings.HasPrefix(a, "--") {
				fmt.Fprintln(stderr, "usage: flint session end [id] [--summary=\"text\"]")
				return 1
			}
			if id != "" {
				fmt.Fprintln(stderr, "usage: flint session end [id] [--summary=\"text\"]")
				return 1
			}
			id = strings.TrimSpace(a)
		}
		if id == "" {
			sess := svc.SessionStore()
			if sess == nil {
				fmt.Fprintln(stderr, "session end failed: store does not support session operations")
				return 1
			}
			activeID, err := sess.ActiveSessionID()
			if err != nil {
				fmt.Fprintf(stderr, "session end failed: %v\n", err)
				return 1
			}
			id = activeID
		}
		if summary != "" {
			if err := svc.SessionSummary(id, summary); err != nil {
				fmt.Fprintf(stderr, "session summary failed: %v\n", err)
				return 1
			}
		}
		if err := svc.SessionEnd(id); err != nil {
			fmt.Fprintf(stderr, "session end failed: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "session ended %s\n", id)
		return 0
	case "summary":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "usage: flint session summary <id> \"text\"")
			return 1
		}
		if len(args) < 3 {
			fmt.Fprintln(stderr, "usage: flint session summary <id> \"text\"")
			return 1
		}
		id := strings.TrimSpace(args[1])
		text := strings.TrimSpace(strings.Join(args[2:], " "))
		if text == "" {
			fmt.Fprintln(stderr, "summary text cannot be empty")
			return 1
		}
		if err := svc.SessionSummary(id, text); err != nil {
			fmt.Fprintf(stderr, "session summary failed: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "session summary %s\n", id)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown session subcommand %q (expected start|end|summary)\n", args[0])
		return 1
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "flint commands:")
	fmt.Fprintln(w, "  flint init  Initialize flint: configure runtime, AI model, domain, and persona")
	fmt.Fprintln(w, "  flint save \"note\"")
	fmt.Fprintln(w, "  flint recall [--type=...] [--since=YYYY-MM-DD] [--until=YYYY-MM-DD] \"query\"")
	fmt.Fprintln(w, "  flint context [--n=10] [--scope=...]")
	fmt.Fprintln(w, "  flint list [limit] [--all-projects]")
	fmt.Fprintln(w, "  flint get <id>")
	fmt.Fprintln(w, "  flint update <id> \"new content\"")
	fmt.Fprintln(w, "  flint delete <id>")
	fmt.Fprintln(w, "  flint session <start|end|summary> [id] [text]")
	fmt.Fprintln(w, "  flint behavior <list|show|dismiss|preview>")
	fmt.Fprintln(w, "  flint import [--dry-run] <file.json>")
	fmt.Fprintln(w, "  flint backup <path>")
	fmt.Fprintln(w, "  flint restore <path>")
	fmt.Fprintln(w, "  flint doctor")
	fmt.Fprintln(w, "  flint mcp")
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if strings.TrimSpace(a) == flag {
			return true
		}
	}
	return false
}

// parseDateFlag accepts YYYY-MM-DD (interpreted as UTC midnight) or RFC3339.
// The shorter form is the documented surface — RFC3339 is accepted as an
// escape hatch for tests/automation that need wall-clock precision.
func parseDateFlag(raw string) (time.Time, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return time.Time{}, errors.New("empty date")
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("expected YYYY-MM-DD or RFC3339, got %q", v)
	}
	return t.UTC(), nil
}

// runParity dispatches `flint parity <subcommand>`. Subcommands:
//   - scorecard:  computes the parity verdict from supplied dimension
//     signals and prints the canonical JSON contract.
//   - continuity: replays the knowledge-continuity fixture against the
//     live store, writes baseline.json, and prints the same JSON.
//
// Each subcommand keeps its own flag parser so unknown flags fail
// loudly (a silent skip would let a typo zero a dimension and quietly
// skew the scorecard verdict).
func runParity(svc *Service, args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: flint parity <scorecard|continuity>")
		return 1
	}
	switch args[1] {
	case "scorecard":
		return runParityScorecard(args[2:], stdout, stderr)
	case "continuity":
		return runParityContinuity(svc, args[2:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown parity subcommand %q (valid: scorecard, continuity)\n", args[1])
		return 1
	}
}

// parityScorecardFlags is the set of int flags accepted by
// `flint parity scorecard`. They map 1:1 to model.ScorecardSignals
// fields so changes to the contract surface here as compile errors.
type parityScorecardFlags struct {
	coreOps                int
	metadataRetrieval      int
	sessionWorkflow        int
	importExportBackup     int
	reliabilityOperability int
	knowledgeContinuity    int
	uxAPIContract          int
	soakDays               int
}

// parseParityScorecardFlags parses the supported `--key=value` flags into
// a parityScorecardFlags value. Unknown flags surface a usage error so
// typos fail loudly instead of silently scoring 0 and skewing the verdict.
func parseParityScorecardFlags(args []string) (parityScorecardFlags, error) {
	f := parityScorecardFlags{}
	known := map[string]*int{
		"--core-ops":                &f.coreOps,
		"--metadata-retrieval":      &f.metadataRetrieval,
		"--session-workflow":        &f.sessionWorkflow,
		"--import-export-backup":    &f.importExportBackup,
		"--reliability-operability": &f.reliabilityOperability,
		"--knowledge-continuity":    &f.knowledgeContinuity,
		"--ux-api-contract":         &f.uxAPIContract,
		"--soak-days":               &f.soakDays,
	}
	for _, raw := range args {
		key, val, ok := strings.Cut(raw, "=")
		if !ok {
			return f, fmt.Errorf("flag %q must use --key=value form", raw)
		}
		ptr, found := known[key]
		if !found {
			return f, fmt.Errorf("unknown flag %q", key)
		}
		n, err := strconv.Atoi(val)
		if err != nil {
			return f, fmt.Errorf("flag %s expects integer, got %q", key, val)
		}
		*ptr = n
	}
	return f, nil
}

func runParityScorecard(args []string, stdout, stderr io.Writer) int {
	flags, err := parseParityScorecardFlags(args)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	v := parity.ComputeScorecard(model.ScorecardSignals{
		CoreOps:                flags.coreOps,
		MetadataRetrieval:      flags.metadataRetrieval,
		SessionWorkflow:        flags.sessionWorkflow,
		ImportExportBackup:     flags.importExportBackup,
		ReliabilityOperability: flags.reliabilityOperability,
		KnowledgeContinuity:    flags.knowledgeContinuity,
		UXAPIContract:          flags.uxAPIContract,
		SoakDays:               flags.soakDays,
	})
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(stderr, "encode scorecard: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, string(b))
	return 0
}

// runParityContinuity executes `flint parity continuity --fixture=<path> --out=<path>`.
// Replays the fixture suite through the live store, writes baseline.json
// to --out, and prints the same baseline as JSON on stdout so the
// runbook can pipe it into jq or diff against a previous baseline.
//
// Both flags are required: a missing fixture should fail loudly rather
// than silently producing a zero-row baseline that would skew the
// scorecard's KnowledgeContinuity dimension to zero.
func runParityContinuity(svc *Service, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("parity continuity", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fixture := fs.String("fixture", "", "path to fixture queries.json (required)")
	out := fs.String("out", "", "path to write baseline.json (required)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *fixture == "" {
		fmt.Fprintln(stderr, "parity continuity: --fixture is required")
		return 1
	}
	if *out == "" {
		fmt.Fprintln(stderr, "parity continuity: --out is required")
		return 1
	}
	baseline, err := svc.RunContinuityHarness(*fixture, *out)
	if err != nil {
		fmt.Fprintf(stderr, "parity continuity: %v\n", err)
		return 1
	}
	body, err := json.Marshal(baseline)
	if err != nil {
		fmt.Fprintf(stderr, "encode baseline: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, string(body))
	return 0
}
