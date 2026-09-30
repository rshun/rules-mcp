package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"rules-mcp/internal/rules"
)

type App struct {
	config Config
	runner Runner
	mu     sync.Mutex
}

func New(c Config) (*App, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(c.Repository)
	if err != nil || filepath.Clean(real) != filepath.Clean(c.Repository) {
		return nil, fmt.Errorf("repository is missing or contains symlinks")
	}
	c.Repository = filepath.Clean(c.Repository)
	info, err := os.Lstat(filepath.Join(c.Repository, ".git"))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("a dedicated Git checkout with a .git directory is required")
	}
	return &App{config: c, runner: ExecRunner{Directory: c.Repository}}, nil
}

type Arguments struct {
	Name             string   `json:"name,omitempty"`
	Add              []string `json:"add,omitempty"`
	Remove           []string `json:"remove,omitempty"`
	ExpectedRevision string   `json:"expected_revision,omitempty"`
	Offset           int      `json:"offset,omitempty"`
	Limit            int      `json:"limit,omitempty"`
}

func (a *App) Execute(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	if !a.mu.TryLock() {
		return nil, fmt.Errorf("another rules-mcp operation is running; retry after it completes")
	}
	defer a.mu.Unlock()
	lock, err := checkedPath(a.config.Repository, filepath.Join(".git", "rules-mcp.lock"))
	if err != nil {
		return nil, err
	}
	unlock, err := acquireLock(lock)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	var args Arguments
	if err := Decode(raw, &args); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(a.config.TimeoutSeconds)*time.Second)
	defer cancel()
	if err := a.checkBranch(ctx); err != nil {
		return nil, err
	}
	switch name {
	case "rules_list":
		return a.list()
	case "rules_read":
		return a.read(args)
	case "rules_preview":
		return a.preview(args)
	case "rules_apply":
		return a.apply(ctx, args)
	case "rules_status":
		return a.status(ctx)
	case "rules_resume":
		return a.resume(ctx)
	default:
		return nil, fmt.Errorf("unknown tool")
	}
}

func (a *App) checkBranch(ctx context.Context) error {
	root, err := a.git(ctx, "rev-parse", "--show-toplevel")
	if err != nil || filepath.Clean(filepath.FromSlash(root)) != a.config.Repository {
		return fmt.Errorf("configured path is not the repository root")
	}
	branch, err := a.git(ctx, "branch", "--show-current")
	if err != nil || branch != a.config.Branch {
		return fmt.Errorf("checkout must be on the configured working branch")
	}
	return nil
}

func (a *App) list() (any, error) {
	entries, err := os.ReadDir(a.config.Repository)
	if err != nil {
		return nil, fmt.Errorf("cannot list repository")
	}
	names := []string{}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".yaml") {
			name := strings.TrimSuffix(entry.Name(), ".yaml")
			if rules.NamePattern.MatchString(name) {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return map[string]any{"names": names}, nil
}

func (a *App) files(name string) ([]byte, []byte, error) {
	if !rules.NamePattern.MatchString(name) {
		return nil, nil, fmt.Errorf("name must be a basename without extension, dots or path separators")
	}
	y, err := readOptional(a.config.Repository, name+".yaml")
	if err != nil {
		return nil, nil, err
	}
	j, err := readOptional(a.config.Repository, filepath.Join("json", name+".json"))
	return y, j, err
}

func (a *App) read(args Arguments) (any, error) {
	y, j, err := a.files(args.Name)
	if err != nil {
		return nil, err
	}
	if y == nil {
		return nil, fmt.Errorf("YAML file does not exist; preview can create it")
	}
	if hasSensitive(y) || hasSensitive(j) {
		return nil, fmt.Errorf("credential-like content detected; refusing to display it")
	}
	d, err := rules.Parse(y)
	if err != nil {
		return nil, err
	}
	if args.Offset < 0 || args.Offset > len(d.Rules) || args.Limit < 0 || args.Limit > 500 {
		return nil, fmt.Errorf("invalid pagination; limit is at most 500")
	}
	if args.Limit == 0 {
		args.Limit = 100
	}
	end := min(args.Offset+args.Limit, len(d.Rules))
	items := []string{}
	for _, r := range d.Rules[args.Offset:end] {
		items = append(items, r.String())
	}
	generated, conversionErr := d.JSON()
	conversionError := ""
	if conversionErr != nil {
		conversionError = conversionErr.Error()
	}
	validationErr := rules.ValidateJSON(j)
	validationError := ""
	if validationErr != nil {
		validationError = validationErr.Error()
	}
	return map[string]any{"name": args.Name, "rules": items, "total": len(d.Rules), "offset": args.Offset, "next_offset": end, "revision": rules.Revision(y, j), "json_in_sync": jsonEqual(generated, j), "json_valid": validationErr == nil, "json_validation_error": validationError, "conversion_error": conversionError, "warnings": warningSummary(d)}, nil
}

func jsonEqual(a, b []byte) bool {
	if rules.ValidateJSON(a) != nil || rules.ValidateJSON(b) != nil {
		return false
	}
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}
func warningSummary(d rules.Document) map[string]any {
	return map[string]any{"count": len(d.Warnings), "first": d.Warnings[:min(len(d.Warnings), 10)]}
}

type Plan struct {
	Name      string         `json:"name"`
	Revision  string         `json:"revision"`
	Changed   bool           `json:"changed"`
	RuleCount int            `json:"rule_count"`
	Warnings  map[string]any `json:"warnings"`
	Add       []string       `json:"add"`
	Remove    []string       `json:"remove"`
	yaml      []byte
	js        []byte
	oldYAML   []byte
	oldJSON   []byte
}

func (a *App) plan(args Arguments) (Plan, error) {
	p := Plan{Name: args.Name, Add: args.Add, Remove: args.Remove}
	if len(args.Add)+len(args.Remove) > 2000 {
		return p, fmt.Errorf("at most 2000 edits per operation")
	}
	y, j, err := a.files(args.Name)
	if err != nil {
		return p, err
	}
	p.oldYAML, p.oldJSON = y, j
	p.Revision = rules.Revision(y, j)
	if y == nil {
		y = []byte("payload:\n")
	}
	d, err := rules.Parse(y)
	if err != nil {
		return p, err
	}
	p.yaml, err = d.Apply(rules.Edit{Add: args.Add, Remove: args.Remove})
	if err != nil {
		return p, err
	}
	// Preserve byte-identical originals for no-op edits, including BOM/newlines.
	if len(args.Add) == 0 && len(args.Remove) == 0 && p.oldYAML != nil {
		p.yaml = p.oldYAML
	}
	updated, err := rules.Parse(p.yaml)
	if err != nil {
		return p, err
	}
	p.js, err = updated.JSON()
	if err != nil {
		return p, err
	}
	if hasSensitive(p.yaml) || hasSensitive(p.js) {
		return p, fmt.Errorf("credential-like content detected; operation refused without displaying it")
	}
	if jsonEqual(p.js, j) {
		p.js = j
	}
	p.Changed = !bytes.Equal(p.yaml, p.oldYAML) || !bytes.Equal(p.js, p.oldJSON)
	p.RuleCount = len(updated.Rules)
	p.Warnings = warningSummary(d)
	return p, nil
}
func (a *App) preview(args Arguments) (any, error) { return a.plan(args) }

func (a *App) clean(ctx context.Context) error {
	s, err := a.git(ctx, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}
	if s != "" {
		return fmt.Errorf("repository has existing changes; resolve them before continuing")
	}
	return nil
}

func (a *App) fetch(ctx context.Context) error {
	args := []string{"fetch", "--no-tags", a.config.Remote, "+refs/heads/" + a.config.Branch + ":refs/remotes/" + a.config.Remote + "/" + a.config.Branch}
	if a.config.PublishBranch != "" {
		args = append(args, "+refs/heads/"+a.config.PublishBranch+":refs/remotes/"+a.config.Remote+"/"+a.config.PublishBranch)
	}
	_, err := a.git(ctx, args...)
	return err
}

func (a *App) refresh(ctx context.Context) error {
	if err := a.clean(ctx); err != nil {
		return err
	}
	if err := a.fetch(ctx); err != nil {
		return fmt.Errorf("fetch failed: %w", err)
	}
	remote := a.config.Remote + "/" + a.config.Branch
	// A pending MCP commit must be resumed, never mixed with another edit.
	if _, err := a.git(ctx, "merge-base", "--is-ancestor", "HEAD", remote); err != nil {
		return fmt.Errorf("local branch has untracked commits or has diverged; cannot publish them implicitly")
	}
	if _, err := a.git(ctx, "pull", "--ff-only", "--no-rebase", a.config.Remote, a.config.Branch); err != nil {
		return fmt.Errorf("fast-forward pull failed: %w", err)
	}
	if a.config.PublishBranch != "" {
		target := a.config.Remote + "/" + a.config.PublishBranch
		if _, err := a.git(ctx, "merge-base", "--is-ancestor", target, "HEAD"); err != nil {
			return fmt.Errorf("publish branch is ahead or diverged; integrate it into the development branch and push that branch before retrying")
		}
	}
	return nil
}

type Journal struct {
	Name          string `json:"name"`
	Branch        string `json:"branch"`
	Remote        string `json:"remote"`
	PublishBranch string `json:"publish_branch"`
	Base          string `json:"base"`
	Commit        string `json:"commit,omitempty"`
	YAML          []byte `json:"yaml"`
	JSON          []byte `json:"json"`
	OldYAMLHash   string `json:"old_yaml_hash"`
	OldJSONHash   string `json:"old_json_hash"`
	Phase         string `json:"phase"`
}

func (a *App) journalPath() (string, error) {
	return checkedPath(a.config.Repository, filepath.Join(".git", "rules-mcp-journal.json"))
}
func (a *App) journal() (*Journal, error) {
	p, err := a.journalPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read operation journal")
	}
	// Accept the retired v0.1 journal field without retaining it in new journals.
	var legacy struct {
		Journal
		Synced *bool `json:"synced,omitempty"`
	}
	if err = Decode(b, &legacy); err != nil {
		return nil, fmt.Errorf("invalid operation journal; manual inspection required")
	}
	j := legacy.Journal
	if !rules.NamePattern.MatchString(j.Name) {
		return nil, fmt.Errorf("invalid operation journal name")
	}
	if j.Phase != "complete" && (j.Branch != a.config.Branch || j.Remote != a.config.Remote || j.PublishBranch != a.config.PublishBranch) {
		return nil, fmt.Errorf("configuration differs from pending operation")
	}
	return &j, nil
}
func (a *App) save(j *Journal) error {
	p, err := a.journalPath()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(p, b, 0600)
}
func (a *App) status(ctx context.Context) (any, error) {
	j, err := a.journal()
	if err != nil {
		return nil, err
	}
	head, err := a.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	state := "idle"
	commit := ""
	if j != nil {
		state = j.Phase
		commit = j.Commit
	}
	return map[string]any{"branch": a.config.Branch, "publish_branch": a.config.PublishBranch, "head": head, "phase": state, "commit": commit}, nil
}

func (a *App) apply(ctx context.Context, args Arguments) (any, error) {
	j, err := a.journal()
	if err != nil {
		return nil, err
	}
	if j != nil && j.Phase != "complete" {
		return nil, fmt.Errorf("an unfinished operation exists; use rules_status and rules_resume")
	}
	if len(args.ExpectedRevision) != 64 {
		return nil, fmt.Errorf("expected_revision from rules_preview is required")
	}
	if _, err = a.plan(args); err != nil {
		return nil, err
	}
	if err = a.refresh(ctx); err != nil {
		return nil, err
	}
	p, err := a.plan(args)
	if err != nil {
		return nil, err
	}
	if p.Revision != args.ExpectedRevision {
		return nil, fmt.Errorf("files changed since preview or during pull; preview again")
	}
	if !p.Changed {
		return map[string]any{"changed": false, "phase": "unchanged"}, nil
	}
	head, err := a.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	j = &Journal{Name: args.Name, Branch: a.config.Branch, Remote: a.config.Remote, PublishBranch: a.config.PublishBranch, Base: head, YAML: p.yaml, JSON: p.js, OldYAMLHash: digest(p.oldYAML), OldJSONHash: digest(p.oldJSON), Phase: "prepared"}
	if err = a.save(j); err != nil {
		return nil, err
	}
	return a.finish(ctx, j)
}

func (a *App) resume(ctx context.Context) (any, error) {
	j, err := a.journal()
	if err != nil {
		return nil, err
	}
	if j == nil {
		return nil, fmt.Errorf("no operation to resume")
	}
	if j.Phase == "complete" {
		return a.status(ctx)
	}
	return a.finish(ctx, j)
}

func (a *App) finish(ctx context.Context, j *Journal) (any, error) {
	err := a.advance(ctx, j)
	result := map[string]any{"changed": true, "phase": j.Phase, "commit": j.Commit, "pushed": j.Phase == "pushed" || j.Phase == "complete"}
	if err != nil {
		result["error"] = err.Error()
		result["recovery"] = "Inspect rules_status, correct the cause, then call rules_resume. Do not repeat rules_apply."
		return result, err
	}
	return result, nil
}

func (a *App) advance(ctx context.Context, j *Journal) error {
	// Revalidate journals from disk, including committed operations, before any
	// file replacement or push. Older binaries did not enforce this JSON profile.
	if err := rules.ValidateJSON(j.JSON); err != nil {
		return fmt.Errorf("pending JSON rule-set failed validation: %w", err)
	}
	document, err := rules.Parse(j.YAML)
	if err != nil {
		return fmt.Errorf("pending YAML failed validation: %w", err)
	}
	generated, err := document.JSON()
	if err != nil {
		return fmt.Errorf("pending rules failed validation: %w", err)
	}
	if !jsonEqual(generated, j.JSON) {
		return fmt.Errorf("pending JSON does not match its YAML; manual inspection required")
	}
	head, err := a.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	paths := []string{j.Name + ".yaml", "json/" + j.Name + ".json"}
	if j.Commit == "" && head != j.Base {
		// Recover a crash after commit but before journal persistence.
		parent, e := a.git(ctx, "rev-parse", "HEAD^")
		if e != nil || parent != j.Base {
			return fmt.Errorf("HEAD changed outside the pending operation")
		}
		changed, e := a.git(ctx, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
		if e != nil || !onlyPaths(changed, paths) {
			return fmt.Errorf("unexpected committed files; manual inspection required")
		}
		for i, data := range [][]byte{j.YAML, j.JSON} {
			b, e := a.runner.Run(ctx, "git", []string{"show", "HEAD:" + paths[i]}, nil)
			if e != nil || !bytes.Equal(b, data) {
				return fmt.Errorf("commit does not match pending operation")
			}
		}
		j.Commit = head
		j.Phase = "committed"
		if err = a.save(j); err != nil {
			return err
		}
	}
	if j.Commit == "" {
		s, err := a.git(ctx, "status", "--porcelain", "--untracked-files=all")
		if err != nil {
			return err
		}
		for _, line := range strings.Split(s, "\n") {
			if line == "" {
				continue
			}
			// TrimSpace in git() removes the first status column of the first line.
			file := strings.TrimSpace(line[min(2, len(line)):])
			if file != paths[0] && file != paths[1] {
				return fmt.Errorf("unrelated worktree changes block recovery")
			}
		}
		for i, data := range [][]byte{j.YAML, j.JSON} {
			oldHash := j.OldYAMLHash
			if i == 1 {
				oldHash = j.OldJSONHash
			}
			current, e := readOptional(a.config.Repository, filepath.FromSlash(paths[i]))
			if e != nil {
				return e
			}
			if digest(current) != oldHash && !bytes.Equal(current, data) {
				return fmt.Errorf("managed file changed outside pending operation")
			}
		}
		for i, data := range [][]byte{j.YAML, j.JSON} {
			p, e := checkedPath(a.config.Repository, filepath.FromSlash(paths[i]))
			if e != nil {
				return e
			}
			if err = atomicWrite(p, data, 0644); err != nil {
				return err
			}
		}
		j.Phase = "written"
		if err = a.save(j); err != nil {
			return err
		}
		if _, err = a.git(ctx, append([]string{"add", "--"}, paths...)...); err != nil {
			return err
		}
		staged, err := a.git(ctx, "diff", "--cached", "--name-only")
		if err != nil {
			return err
		}
		if !onlyPaths(staged, paths) {
			return fmt.Errorf("unexpected staged files; commit refused")
		}
		for i, data := range [][]byte{j.YAML, j.JSON} {
			stagedData, e := a.runner.Run(ctx, "git", []string{"show", ":" + paths[i]}, nil)
			if e != nil || hasSensitive(stagedData) || !bytes.Equal(stagedData, data) {
				return fmt.Errorf("staged content differs from validated files or contains credential-like data; commit refused")
			}
		}
		// Commit precisely the inspected index, without re-staging via pathspecs.
		if _, err = a.git(ctx, "commit", "-m", "rules: update "+j.Name); err != nil {
			return fmt.Errorf("commit failed: %w", err)
		}
		j.Commit, err = a.git(ctx, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		j.Phase = "committed"
		if err = a.save(j); err != nil {
			return err
		}
	}
	head, err = a.git(ctx, "rev-parse", "HEAD")
	if err != nil || head != j.Commit {
		return fmt.Errorf("HEAD no longer matches pending commit")
	}
	if err = a.clean(ctx); err != nil {
		return err
	}
	for i, data := range [][]byte{j.YAML, j.JSON} {
		stored, e := a.runner.Run(ctx, "git", []string{"show", j.Commit + ":" + paths[i]}, nil)
		if e != nil || !bytes.Equal(stored, data) {
			return fmt.Errorf("committed content differs from validated files; inspect Git attributes and filters")
		}
	}
	if j.Phase != "pushed" {
		args := []string{"push", "--atomic", a.config.Remote, j.Commit + ":refs/heads/" + a.config.Branch}
		if a.config.PublishBranch != "" {
			args = append(args, j.Commit+":refs/heads/"+a.config.PublishBranch)
		}
		if _, err = a.git(ctx, args...); err != nil {
			return fmt.Errorf("atomic fast-forward push failed; no force push or conflict resolution is attempted: %w", err)
		}
		j.Phase = "pushed"
		if err = a.save(j); err != nil {
			return err
		}
	}
	j.Phase = "complete"
	return a.save(j)
}
func onlyPaths(list string, paths []string) bool {
	if list == "" {
		return false
	}
	for _, p := range strings.Split(list, "\n") {
		if p != paths[0] && p != paths[1] {
			return false
		}
	}
	return true
}
