package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"rules-mcp/internal/rules"
)

func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=" + os.DevNull, "-c", "commit.gpgSign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("test git %s failed: %v: %s", args[0], err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

func fixture(t *testing.T) (*App, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("git is required for integration tests")
	}
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	repo := filepath.Join(base, "repo")
	if err := os.Mkdir(repo, 0755); err != nil {
		t.Fatal(err)
	}
	testGit(t, base, "init", "--bare", "--initial-branch=master", remote)
	testGit(t, repo, "init", "--initial-branch=dev")
	testGit(t, repo, "config", "user.name", "Rules MCP Test")
	testGit(t, repo, "config", "user.email", "rules-mcp@example.invalid")
	testGit(t, repo, "config", "core.autocrlf", "false")
	if err := os.Mkdir(filepath.Join(repo, "json"), 0755); err != nil {
		t.Fatal(err)
	}
	y := []byte("payload:\n# test\n  - DOMAIN,example.com\n")
	d, _ := rules.Parse(y)
	j, _ := d.JSON()
	if err := os.WriteFile(filepath.Join(repo, "sample.yaml"), y, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "json", "sample.json"), j, 0644); err != nil {
		t.Fatal(err)
	}
	testGit(t, repo, "add", "sample.yaml", "json/sample.json")
	testGit(t, repo, "commit", "-m", "initial test fixture")
	testGit(t, repo, "remote", "add", "origin", remote)
	testGit(t, repo, "push", "origin", "HEAD:refs/heads/dev", "HEAD:refs/heads/master")
	a, err := New(Config{Listen: "127.0.0.1:8787", Repository: repo, Branch: "dev", Remote: "origin", PublishBranch: "master", TimeoutSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	return a, remote
}

func runTool(t *testing.T, a *App, name string, args any) (any, error) {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return a.Execute(context.Background(), name, b)
}
func previewEdit(t *testing.T, a *App) Arguments {
	t.Helper()
	args := Arguments{Name: "sample", Add: []string{"DOMAIN-SUFFIX,new.example"}}
	v, err := runTool(t, a, "rules_preview", args)
	if err != nil {
		t.Fatal(err)
	}
	args.ExpectedRevision = v.(Plan).Revision
	return args
}

func TestApplyPublishesBothBranchesWithoutEditingMaster(t *testing.T) {
	a, remote := fixture(t)
	args := previewEdit(t, a)
	v, err := runTool(t, a, "rules_apply", args)
	if err != nil {
		t.Fatal(err)
	}
	if v.(map[string]any)["phase"] != "complete" {
		t.Fatal(v)
	}
	head := testGit(t, a.config.Repository, "rev-parse", "HEAD")
	for _, ref := range []string{"dev", "master"} {
		if got := testGit(t, remote, "rev-parse", ref); got != head {
			t.Fatalf("%s not published", ref)
		}
	}
	if testGit(t, a.config.Repository, "branch", "--show-current") != "dev" {
		t.Fatal("checked out master")
	}
	if state := testGit(t, a.config.Repository, "status", "--porcelain"); state != "" {
		t.Fatalf("dirty after commit: %q", state)
	}
	v, err = runTool(t, a, "rules_apply", previewEdit(t, a))
	if err != nil {
		t.Fatal(err)
	}
	if v.(map[string]any)["changed"] != false {
		t.Fatal("duplicate edit created a commit")
	}
	if testGit(t, a.config.Repository, "rev-parse", "HEAD") != head {
		t.Fatal("no-op committed")
	}
}

type faultRunner struct {
	base       Runner
	failPush   bool
	calls      []string
	beforePush func()
}

func (r *faultRunner) Run(ctx context.Context, name string, args []string, input []byte) ([]byte, error) {
	for _, arg := range args {
		if arg == "push" {
			if r.beforePush != nil {
				fn := r.beforePush
				r.beforePush = nil
				fn()
			}
			r.calls = append(r.calls, "push")
			if r.failPush {
				return nil, fmt.Errorf("injected push failure")
			}
		}
	}
	return r.base.Run(ctx, name, args, input)
}

func TestAtomicPushRejectsBothBranchesWhenMasterAdvances(t *testing.T) {
	a, remote := fixture(t)
	original := testGit(t, remote, "rev-parse", "dev")
	f := &faultRunner{base: a.runner}
	f.beforePush = func() {
		clone := filepath.Join(t.TempDir(), "other")
		testGit(t, filepath.Dir(clone), "clone", "--branch", "master", remote, clone)
		testGit(t, clone, "config", "user.name", "Test")
		testGit(t, clone, "config", "user.email", "test@example.invalid")
		if err := os.WriteFile(filepath.Join(clone, "concurrent.txt"), []byte("concurrent"), 0644); err != nil {
			t.Fatal(err)
		}
		testGit(t, clone, "add", "concurrent.txt")
		testGit(t, clone, "commit", "-m", "concurrent master change")
		testGit(t, clone, "push", "origin", "master")
	}
	a.runner = f
	v, err := runTool(t, a, "rules_apply", previewEdit(t, a))
	if err == nil {
		t.Fatal("race was not rejected")
	}
	if v.(map[string]any)["phase"] != "committed" {
		t.Fatal("wrong failure phase")
	}
	if testGit(t, remote, "rev-parse", "dev") != original {
		t.Fatal("atomic push partially updated dev")
	}
	if testGit(t, remote, "rev-parse", "master") == testGit(t, a.config.Repository, "rev-parse", "HEAD") {
		t.Fatal("overwrote concurrent master commit")
	}
}

func TestResumeCrashBetweenFileReplacements(t *testing.T) {
	a, _ := fixture(t)
	args := previewEdit(t, a)
	p, err := a.plan(args)
	if err != nil {
		t.Fatal(err)
	}
	j := &Journal{Name: args.Name, Branch: a.config.Branch, Remote: a.config.Remote, PublishBranch: a.config.PublishBranch, Base: testGit(t, a.config.Repository, "rev-parse", "HEAD"), YAML: p.yaml, JSON: p.js, OldYAMLHash: digest(p.oldYAML), OldJSONHash: digest(p.oldJSON), Phase: "prepared"}
	if err = a.save(j); err != nil {
		t.Fatal(err)
	}
	if err = atomicWrite(filepath.Join(a.config.Repository, "sample.yaml"), p.yaml, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = runTool(t, a, "rules_resume", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(a.config.Repository, "json", "sample.json"))
	if err != nil || !bytes.Equal(actual, p.js) {
		t.Fatal("JSON not recovered")
	}
}

func TestResumeCrashAfterCommitBeforeJournalSave(t *testing.T) {
	a, _ := fixture(t)
	f := &faultRunner{base: a.runner, failPush: true}
	a.runner = f
	if _, err := runTool(t, a, "rules_apply", previewEdit(t, a)); err == nil {
		t.Fatal("expected injected failure")
	}
	j, err := a.journal()
	if err != nil {
		t.Fatal(err)
	}
	head := j.Commit
	j.Commit = ""
	j.Phase = "written"
	if err = a.save(j); err != nil {
		t.Fatal(err)
	}
	f.failPush = false
	if _, err = runTool(t, a, "rules_resume", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if testGit(t, a.config.Repository, "rev-parse", "HEAD") != head {
		t.Fatal("duplicated recovered commit")
	}
}

func TestResumeFailedPushDoesNotDuplicateCommit(t *testing.T) {
	a, remote := fixture(t)
	original := testGit(t, remote, "rev-parse", "master")
	f := &faultRunner{base: a.runner, failPush: true}
	a.runner = f
	_, err := runTool(t, a, "rules_apply", previewEdit(t, a))
	if err == nil {
		t.Fatal("expected push failure")
	}
	head := testGit(t, a.config.Repository, "rev-parse", "HEAD")
	if head == original || testGit(t, remote, "rev-parse", "master") != original {
		t.Fatal("wrong partial state")
	}
	if _, err = runTool(t, a, "rules_apply", previewEdit(t, a)); err == nil {
		t.Fatal("pending work not blocked")
	}
	f.failPush = false
	if _, err = runTool(t, a, "rules_resume", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if testGit(t, a.config.Repository, "rev-parse", "HEAD") != head || testGit(t, remote, "rev-parse", "master") != head {
		t.Fatal("resume changed commit")
	}
}

func TestDirtyCheckoutAndStalePreviewAreRejected(t *testing.T) {
	a, _ := fixture(t)
	args := previewEdit(t, a)
	if err := os.WriteFile(filepath.Join(a.config.Repository, "unrelated.txt"), []byte("local change"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(t, a, "rules_apply", args); err == nil {
		t.Fatal("dirty checkout accepted")
	}
	if _, err := os.Stat(filepath.Join(a.config.Repository, ".git", "rules-mcp-journal.json")); !os.IsNotExist(err) {
		t.Fatal("created pending operation for dirty checkout")
	}
	// Use another fixture instead of deleting user-like files.
	b, _ := fixture(t)
	args = previewEdit(t, b)
	args.ExpectedRevision = strings.Repeat("0", 64)
	before, _ := os.ReadFile(filepath.Join(b.config.Repository, "sample.yaml"))
	if _, err := runTool(t, b, "rules_apply", args); err == nil {
		t.Fatal("stale preview accepted")
	}
	after, _ := os.ReadFile(filepath.Join(b.config.Repository, "sample.yaml"))
	if !bytes.Equal(before, after) {
		t.Fatal("file modified on stale preview")
	}
}

func TestDivergedPublishBranchStopsBeforeWriting(t *testing.T) {
	a, remote := fixture(t)
	args := previewEdit(t, a)
	clone := filepath.Join(t.TempDir(), "other")
	testGit(t, filepath.Dir(clone), "clone", "--branch", "master", remote, clone)
	testGit(t, clone, "config", "user.name", "Test")
	testGit(t, clone, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(clone, "other.txt"), []byte("master changed"), 0644); err != nil {
		t.Fatal(err)
	}
	testGit(t, clone, "add", "other.txt")
	testGit(t, clone, "commit", "-m", "remote change")
	testGit(t, clone, "push", "origin", "master")
	// Advance dev independently to create a real divergence.
	if err := os.WriteFile(filepath.Join(a.config.Repository, "dev.txt"), []byte("dev changed"), 0644); err != nil {
		t.Fatal(err)
	}
	testGit(t, a.config.Repository, "add", "dev.txt")
	testGit(t, a.config.Repository, "commit", "-m", "dev change")
	testGit(t, a.config.Repository, "push", "origin", "dev")
	before, _ := os.ReadFile(filepath.Join(a.config.Repository, "sample.yaml"))
	if _, err := runTool(t, a, "rules_apply", args); err == nil {
		t.Fatal("divergence accepted")
	}
	after, _ := os.ReadFile(filepath.Join(a.config.Repository, "sample.yaml"))
	if !bytes.Equal(before, after) {
		t.Fatal("edited before conflict check")
	}
}

func TestApplyOnSingleWorkingBranch(t *testing.T) {
	for _, branch := range []string{"master", "main"} {
		t.Run(branch, func(t *testing.T) {
			a, remote := fixture(t)
			repo := a.config.Repository
			testGit(t, repo, "branch", "-m", branch)
			testGit(t, repo, "push", "origin", "HEAD:refs/heads/"+branch)
			a.config.Branch = branch
			a.config.PublishBranch = ""
			if err := a.config.Validate(); err != nil {
				t.Fatal(err)
			}
			untouched := testGit(t, remote, "rev-parse", "dev")
			if _, err := runTool(t, a, "rules_apply", previewEdit(t, a)); err != nil {
				t.Fatal(err)
			}
			head := testGit(t, repo, "rev-parse", "HEAD")
			if head == untouched || testGit(t, remote, "rev-parse", branch) != head {
				t.Fatal("working branch was not committed and pushed")
			}
			if testGit(t, remote, "rev-parse", "dev") != untouched {
				t.Fatal("unconfigured branch changed")
			}
			if testGit(t, repo, "branch", "--show-current") != branch {
				t.Fatal("working branch changed")
			}
		})
	}
}

func TestConfigRejectsExposedHTTPAndInvalidBranches(t *testing.T) {
	a, _ := fixture(t)
	for _, address := range []string{"0.0.0.0:8787", "192.0.2.1:8787", "example.com:8787", ":8787"} {
		c := a.config
		c.Listen = address
		if c.Validate() == nil {
			t.Fatal("public listener accepted")
		}
	}
	for _, branch := range []string{"", "-option", "dev/../master"} {
		c := a.config
		c.Branch = branch
		if c.Validate() == nil {
			t.Fatal("unsafe branch accepted")
		}
	}
	c := a.config
	c.PublishBranch = c.Branch
	if c.Validate() == nil {
		t.Fatal("same working and publish branch accepted")
	}
	for _, name := range []string{"../escape", "sample.yaml", "a/b"} {
		if _, err := runTool(t, a, "rules_read", Arguments{Name: name}); err == nil {
			t.Fatal("unsafe path accepted")
		}
	}
}

func TestSensitiveCommentsAreBlockedWithoutEcho(t *testing.T) {
	a, _ := fixture(t)
	// Construct a synthetic marker, never a real credential.
	data := []byte("payload:\n# API_KEY=" + "dummy_test_value" + "\n  - DOMAIN,example.com\n")
	if err := os.WriteFile(filepath.Join(a.config.Repository, "sample.yaml"), data, 0644); err != nil {
		t.Fatal(err)
	}
	_, err := runTool(t, a, "rules_preview", Arguments{Name: "sample"})
	if err == nil || strings.Contains(err.Error(), "dummy_test_value") {
		t.Fatal("secret guard missing or echoes content")
	}
}

func TestRepositoryLockBlocksAnotherInstance(t *testing.T) {
	a, _ := fixture(t)
	path, err := checkedPath(a.config.Repository, filepath.Join(".git", "rules-mcp.lock"))
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := acquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	b, err := New(a.config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = runTool(t, b, "rules_status", map[string]any{}); err == nil {
		t.Fatal("second instance acquired repository lock")
	}
}

func TestLegacyJournalAfterPushCompletesWithoutRepublishing(t *testing.T) {
	for _, phase := range []string{"pushed", "complete"} {
		t.Run(phase, func(t *testing.T) {
			a, _ := fixture(t)
			if _, err := runTool(t, a, "rules_apply", previewEdit(t, a)); err != nil {
				t.Fatal(err)
			}
			journalPath, err := a.journalPath()
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			var legacy map[string]any
			if err = json.Unmarshal(data, &legacy); err != nil {
				t.Fatal(err)
			}
			legacy["phase"] = phase
			legacy["synced"] = false
			data, err = json.Marshal(legacy)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(journalPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			f := &faultRunner{base: a.runner, failPush: true}
			a.runner = f
			result, err := runTool(t, a, "rules_resume", map[string]any{})
			if err != nil {
				t.Fatal(err)
			}
			if result.(map[string]any)["phase"] != "complete" || len(f.calls) != 0 {
				t.Fatal("unexpected recovery result")
			}
			status, err := runTool(t, a, "rules_status", map[string]any{})
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"openwrt_enabled", "openwrt_synced"} {
				if _, ok := status.(map[string]any)[key]; ok {
					t.Fatal("retired status field returned")
				}
			}
			if phase == "pushed" {
				data, err = os.ReadFile(journalPath)
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(data, []byte(`"synced"`)) {
					t.Fatal("retired journal field persisted")
				}
			}
		})
	}
}

func TestRemovedConfigFieldIsRejected(t *testing.T) {
	var c Config
	if err := Decode([]byte(`{"openwrt":{"enabled":false}}`), &c); err == nil {
		t.Fatal("retired config silently accepted")
	}
}

func pushRemoteFiles(t *testing.T, remote, branch string, files map[string][]byte, message string) string {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "remote-edit")
	testGit(t, filepath.Dir(clone), "clone", "--branch", branch, remote, clone)
	testGit(t, clone, "config", "user.name", "Test")
	testGit(t, clone, "config", "user.email", "test@example.invalid")
	for relPath, content := range files {
		target := filepath.Join(clone, filepath.FromSlash(relPath))
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, content, 0644); err != nil {
			t.Fatal(err)
		}
		testGit(t, clone, "add", relPath)
	}
	testGit(t, clone, "commit", "-m", message)
	testGit(t, clone, "push", "origin", branch)
	return testGit(t, clone, "rev-parse", "HEAD")
}

func optionalRef(dir, ref string) (string, bool) {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", ref)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

func TestCompareMatchesAndDetectsRemoteAdvanceWithoutMutatingLocal(t *testing.T) {
	a, remote := fixture(t)
	oldHead := testGit(t, a.config.Repository, "rev-parse", "HEAD")
	v, err := runTool(t, a, "rules_compare", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	got := v.(map[string]any)
	if got["remote"] != "origin" || got["branch"] != "dev" || got["local_head"] != oldHead || got["remote_head"] != oldHead || got["in_sync"] != true || got["hash_format"] != "sha1" || got["publish_branch"] != "master" || got["remote_publish_head"] != oldHead || got["publish_in_sync"] != true {
		t.Fatalf("unexpected initial compare result: %v", got)
	}

	newHead := pushRemoteFiles(t, remote, "dev", map[string][]byte{"remote.txt": []byte("remote change\n")}, "remote dev change")
	beforeRef, beforeExists := optionalRef(a.config.Repository, "refs/remotes/origin/dev")
	v, err = runTool(t, a, "rules_compare", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	got = v.(map[string]any)
	if got["in_sync"] != false || got["local_head"] != oldHead || got["remote_head"] != newHead || got["remote_publish_head"] != oldHead || got["publish_in_sync"] != true {
		t.Fatalf("unexpected out-of-sync compare result: %v", got)
	}
	if testGit(t, a.config.Repository, "rev-parse", "HEAD") != oldHead {
		t.Fatal("compare modified local HEAD")
	}
	afterRef, afterExists := optionalRef(a.config.Repository, "refs/remotes/origin/dev")
	if afterExists != beforeExists || afterRef != beforeRef || afterRef == newHead {
		t.Fatalf("compare mutated local remote-tracking ref: before=(%q,%v) after=(%q,%v)", beforeRef, beforeExists, afterRef, afterExists)
	}
	if _, err := os.Stat(filepath.Join(a.config.Repository, ".git", "rules-mcp-journal.json")); !os.IsNotExist(err) {
		t.Fatal("compare created journal file")
	}

	if err := os.WriteFile(filepath.Join(a.config.Repository, "untracked.txt"), []byte("note"), 0644); err != nil {
		t.Fatal(err)
	}
	if v, err = runTool(t, a, "rules_compare", map[string]any{}); err != nil || v.(map[string]any)["remote_head"] != newHead {
		t.Fatalf("compare should stay read-only with untracked files: v=%v err=%v", v, err)
	}

	b, _ := fixture(t)
	testGit(t, b.config.Repository, "branch", "-m", "master")
	testGit(t, b.config.Repository, "push", "origin", "HEAD:refs/heads/master")
	b.config.Branch = "master"
	b.config.PublishBranch = ""
	v, err = runTool(t, b, "rules_compare", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	single := v.(map[string]any)
	if single["in_sync"] != true || single["local_head"] != single["remote_head"] {
		t.Fatalf("unexpected single-branch compare result: %v", single)
	}
	for _, key := range []string{"publish_branch", "remote_publish_head", "publish_in_sync"} {
		if _, ok := single[key]; ok {
			t.Fatalf("unexpected publish field %s in single-branch compare: %v", key, single)
		}
	}

	testGit(t, b.config.Repository, "branch", "-m", "missing")
	b.config.Branch = "missing"
	if _, err = runTool(t, b, "rules_compare", map[string]any{}); err == nil {
		t.Fatal("missing remote branch accepted")
	}
}

func TestPullFastForwardsCleanCheckoutAndReportsUpToDate(t *testing.T) {
	a, remote := fixture(t)
	oldHead := testGit(t, a.config.Repository, "rev-parse", "HEAD")
	v, err := runTool(t, a, "rules_pull", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	got := v.(map[string]any)
	if got["remote"] != "origin" || got["branch"] != "dev" || got["previous_head"] != oldHead || got["head"] != oldHead || got["updated"] != false {
		t.Fatalf("unexpected initial pull result: %v", got)
	}

	updatedYAML := []byte("payload:\n# test\n  - DOMAIN,example.com\n  - DOMAIN-SUFFIX,remote.example\n")
	d, err := rules.Parse(updatedYAML)
	if err != nil {
		t.Fatal(err)
	}
	updatedJSON, err := d.JSON()
	if err != nil {
		t.Fatal(err)
	}
	newHead := pushRemoteFiles(t, remote, "dev", map[string][]byte{
		"sample.yaml":      updatedYAML,
		"json/sample.json": updatedJSON,
	}, "remote rules update")

	v, err = runTool(t, a, "rules_pull", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	got = v.(map[string]any)
	if got["previous_head"] != oldHead || got["head"] != newHead || got["updated"] != true {
		t.Fatalf("unexpected fast-forward pull result: %v", got)
	}
	if testGit(t, a.config.Repository, "rev-parse", "HEAD") != newHead {
		t.Fatal("local HEAD was not fast-forwarded")
	}
	if state := testGit(t, a.config.Repository, "status", "--porcelain"); state != "" {
		t.Fatalf("dirty worktree after pull: %q", state)
	}
	diskYAML, err := os.ReadFile(filepath.Join(a.config.Repository, "sample.yaml"))
	if err != nil || !bytes.Equal(diskYAML, updatedYAML) {
		t.Fatal("updated YAML not present after pull")
	}
	diskJSON, err := os.ReadFile(filepath.Join(a.config.Repository, "json", "sample.json"))
	if err != nil || !bytes.Equal(diskJSON, updatedJSON) {
		t.Fatal("updated JSON not present after pull")
	}

	cmp, err := runTool(t, a, "rules_compare", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	cmpMap := cmp.(map[string]any)
	if cmpMap["in_sync"] != true || cmpMap["local_head"] != newHead || cmpMap["remote_head"] != newHead || cmpMap["publish_in_sync"] != false {
		t.Fatalf("unexpected compare after pull: %v", cmpMap)
	}

	v, err = runTool(t, a, "rules_pull", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	got = v.(map[string]any)
	if got["previous_head"] != newHead || got["head"] != newHead || got["updated"] != false {
		t.Fatalf("unexpected repeat pull result: %v", got)
	}

	b, remoteB := fixture(t)
	testGit(t, b.config.Repository, "branch", "-m", "master")
	testGit(t, b.config.Repository, "push", "origin", "HEAD:refs/heads/master")
	b.config.Branch = "master"
	b.config.PublishBranch = ""
	newMasterHead := pushRemoteFiles(t, remoteB, "master", map[string][]byte{"single.txt": []byte("ok\n")}, "master update")
	v, err = runTool(t, b, "rules_pull", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	single := v.(map[string]any)
	if single["branch"] != "master" || single["head"] != newMasterHead || single["updated"] != true {
		t.Fatalf("unexpected single-branch pull result: %v", single)
	}
}

func TestPullRejectsDirtyDivergedAndPendingJournal(t *testing.T) {
	a, _ := fixture(t)
	oldHead := testGit(t, a.config.Repository, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(a.config.Repository, "unrelated.txt"), []byte("dirty"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(t, a, "rules_pull", map[string]any{}); err == nil {
		t.Fatal("dirty checkout accepted by rules_pull")
	}
	if testGit(t, a.config.Repository, "rev-parse", "HEAD") != oldHead {
		t.Fatal("dirty rejection modified HEAD")
	}

	b, _ := fixture(t)
	if err := os.WriteFile(filepath.Join(b.config.Repository, "local.txt"), []byte("ahead"), 0644); err != nil {
		t.Fatal(err)
	}
	testGit(t, b.config.Repository, "add", "local.txt")
	testGit(t, b.config.Repository, "commit", "-m", "unpushed local commit")
	aheadHead := testGit(t, b.config.Repository, "rev-parse", "HEAD")
	if _, err := runTool(t, b, "rules_pull", map[string]any{}); err == nil {
		t.Fatal("unpushed local commit accepted by rules_pull")
	}
	if testGit(t, b.config.Repository, "rev-parse", "HEAD") != aheadHead {
		t.Fatal("unpushed check modified HEAD")
	}

	c, remoteC := fixture(t)
	pushRemoteFiles(t, remoteC, "dev", map[string][]byte{"remote.txt": []byte("remote")}, "remote commit")
	if err := os.WriteFile(filepath.Join(c.config.Repository, "local.txt"), []byte("local"), 0644); err != nil {
		t.Fatal(err)
	}
	testGit(t, c.config.Repository, "add", "local.txt")
	testGit(t, c.config.Repository, "commit", "-m", "diverged local commit")
	divergedHead := testGit(t, c.config.Repository, "rev-parse", "HEAD")
	if _, err := runTool(t, c, "rules_pull", map[string]any{}); err == nil {
		t.Fatal("diverged branch accepted by rules_pull")
	}
	if testGit(t, c.config.Repository, "rev-parse", "HEAD") != divergedHead {
		t.Fatal("diverged rejection modified HEAD")
	}

	d, _ := fixture(t)
	f := &faultRunner{base: d.runner, failPush: true}
	d.runner = f
	if _, err := runTool(t, d, "rules_apply", previewEdit(t, d)); err == nil {
		t.Fatal("expected push failure")
	}
	d.runner = f.base
	pendingHead := testGit(t, d.config.Repository, "rev-parse", "HEAD")
	_, err := runTool(t, d, "rules_pull", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "unfinished operation") {
		t.Fatalf("pending journal not blocked: %v", err)
	}
	if testGit(t, d.config.Repository, "rev-parse", "HEAD") != pendingHead {
		t.Fatal("pending journal rejection modified HEAD")
	}
}

func TestParseRemoteHeadsAndHashHelpers(t *testing.T) {
	sha1 := strings.Repeat("a", 40)
	sha256 := strings.Repeat("0123456789abcdef", 4)
	if !isHexHash(sha1) || !isHexHash(sha256) {
		t.Fatal("valid commit hashes rejected")
	}
	if hashFormat(sha1) != "sha1" || hashFormat(sha256) != "sha256" {
		t.Fatal("wrong hash format detected")
	}
	for _, bad := range []string{"", strings.Repeat("a", 39), strings.Repeat("a", 41), strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("A", 40), strings.Repeat("g", 40)} {
		if isHexHash(bad) {
			t.Fatalf("invalid hash accepted: %q", bad)
		}
	}
	refs, err := parseRemoteHeads("")
	if err != nil || len(refs) != 0 {
		t.Fatalf("empty ls-remote failed: refs=%v err=%v", refs, err)
	}
	refs, err = parseRemoteHeads(sha1 + "\trefs/heads/dev\r\n" + sha256 + "\trefs/heads/master\n")
	if err != nil || refs["refs/heads/dev"] != sha1 || refs["refs/heads/master"] != sha256 {
		t.Fatalf("unexpected parsed refs: refs=%v err=%v", refs, err)
	}
	for _, malformed := range []string{"bad-line", sha1 + " refs/heads/dev", "zzzz\trefs/heads/dev", sha1 + "\t"} {
		if _, err := parseRemoteHeads(malformed); err == nil {
			t.Fatalf("malformed ls-remote line accepted: %q", malformed)
		}
	}
}
