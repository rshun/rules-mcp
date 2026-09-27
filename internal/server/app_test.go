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
	failSSH    bool
	calls      []string
	sshArgs    []string
	sshInput   []byte
	beforePush func()
}

func (r *faultRunner) Run(ctx context.Context, name string, args []string, input []byte) ([]byte, error) {
	if name == "ssh" {
		r.calls = append(r.calls, "ssh")
		r.sshArgs = args
		r.sshInput = input
		if r.failSSH {
			return nil, fmt.Errorf("injected SSH failure")
		}
		return nil, nil
	}
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

func TestResumeSSHFailureAndSafeScript(t *testing.T) {
	a, _ := fixture(t)
	a.config.OpenWrt = OpenWrt{Enabled: true, Host: "router.example.invalid", User: "rules", Port: 2222, IdentityFile: "/unused/test-key", KnownHostsFile: "/unused/known-hosts", JSONDirectory: "/srv/rules dir'quoted"}
	f := &faultRunner{base: a.runner, failSSH: true}
	a.runner = f
	v, err := runTool(t, a, "rules_apply", previewEdit(t, a))
	if err == nil || v.(map[string]any)["phase"] != "pushed" {
		t.Fatalf("expected pushed state: %v %v", v, err)
	}
	if _, err = New(a.config); err != nil && filepath.IsAbs("/unused/test-key") {
		t.Fatal(err)
	}
	f.failSSH = false
	if _, err = runTool(t, a, "rules_resume", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if strings.Count(strings.Join(f.calls, ","), "push") != 1 {
		t.Fatal("pushed a second time on SSH retry")
	}
	script := f.sshArgs[len(f.sshArgs)-1]
	if !strings.Contains(script, "sha256sum -c") || !strings.Contains(script, "set -C") || !strings.Contains(script, shellQuote(a.config.OpenWrt.JSONDirectory)) || strings.Contains(script, "restart") || strings.Contains(script, ".yaml") {
		t.Fatal("unsafe sync script")
	}
	if !json.Valid(f.sshInput) {
		t.Fatal("not JSON")
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

func TestConfigRejectsExposedHTTPAndProtectedBranches(t *testing.T) {
	a, _ := fixture(t)
	for _, address := range []string{"0.0.0.0:8787", "192.0.2.1:8787", "example.com:8787", ":8787"} {
		c := a.config
		c.Listen = address
		if c.Validate() == nil {
			t.Fatal("public listener accepted")
		}
	}
	for _, branch := range []string{"master", "main", "-option", "dev/../master"} {
		c := a.config
		c.Branch = branch
		if c.Validate() == nil {
			t.Fatal("unsafe branch accepted")
		}
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
