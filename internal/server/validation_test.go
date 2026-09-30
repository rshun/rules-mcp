package server

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rules-mcp/internal/rules"
)

func TestReadJSONValidationDiagnostics(t *testing.T) {
	a, _ := fixture(t)
	for _, tt := range []struct{ input, message string }{
		{`{"version":1,"rules":[`, "JSON syntax"},
		{`{"version":2,"rules":[]}`, "integer 1"},
		{`{"version":1,"rules":[{"ip_cidr":["192.0.2.0/99"]}]}`, "rules[0]: ip_cidr[0]"},
	} {
		if err := os.WriteFile(filepath.Join(a.config.Repository, "json/sample.json"), []byte(tt.input), 0644); err != nil {
			t.Fatal(err)
		}
		result, err := runTool(t, a, "rules_read", Arguments{Name: "sample"})
		if err != nil {
			t.Fatal(err)
		}
		read := result.(map[string]any)
		if read["json_valid"] != false || read["json_in_sync"] != false || !strings.Contains(read["json_validation_error"].(string), tt.message) {
			t.Fatalf("unexpected validation diagnostic: %v", read)
		}
	}
}

func TestApplyRepairsDuplicateJSONFieldsInsteadOfPreservingThem(t *testing.T) {
	a, remote := fixture(t)
	yamlPath := filepath.Join(a.config.Repository, "sample.yaml")
	jsonPath := filepath.Join(a.config.Repository, "json/sample.json")
	originalYAML, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	// encoding/json would silently discard the first domain field. The second
	// equals the YAML-derived value, which used to keep the broken file intact.
	broken := []byte(`{"version":1,"rules":[{"domain":["other.example"],"domain":["example.com"]}]}`)
	if err = os.WriteFile(jsonPath, broken, 0644); err != nil {
		t.Fatal(err)
	}
	testGit(t, a.config.Repository, "add", "json/sample.json")
	testGit(t, a.config.Repository, "commit", "-m", "legacy duplicate JSON fixture")
	testGit(t, a.config.Repository, "push", "origin", "HEAD:refs/heads/dev", "HEAD:refs/heads/master")
	args := Arguments{Name: "sample"}
	v, err := runTool(t, a, "rules_preview", args)
	if err != nil {
		t.Fatal(err)
	}
	plan := v.(Plan)
	if !plan.Changed {
		t.Fatal("duplicate fields falsely treated as valid and synchronized")
	}
	args.ExpectedRevision = plan.Revision
	if _, err = runTool(t, a, "rules_apply", args); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(jsonPath)
	if err != nil || rules.ValidateJSON(stored) != nil || !bytes.Equal(stored, plan.js) {
		t.Fatal("corrected JSON was not written")
	}
	yaml, err := os.ReadFile(yamlPath)
	if err != nil || !bytes.Equal(yaml, originalYAML) {
		t.Fatal("JSON repair changed the YAML source")
	}
	head := testGit(t, a.config.Repository, "rev-parse", "HEAD")
	for _, ref := range []string{"dev", "master"} {
		if testGit(t, remote, "rev-parse", ref) != head {
			t.Fatal("corrected JSON was not published")
		}
	}
}

func TestResumeRevalidatesJSONAndYAMLBeforeWriteOrPush(t *testing.T) {
	for _, tt := range []struct {
		name, input string
		committed   bool
	}{
		{"invalid_before_write", `{"version":1,"rules":[{"ip_cidr":["invalid"]}]}`, false},
		{"duplicate_before_write", `{"version":1,"rules":[{"domain":["other.example"],"domain":["example.com"]}]}`, false},
		{"mismatch_before_write", `{"version":1,"rules":[{"domain":["other.example"]}]}`, false},
		{"invalid_before_push", `{"version":1,"rules":[{"ip_cidr":["192.0.2.0/99"]}]}`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, remote := fixture(t)
			yaml, originalJSON, err := a.files("sample")
			if err != nil {
				t.Fatal(err)
			}
			base := testGit(t, a.config.Repository, "rev-parse", "HEAD")
			j := &Journal{Name: "sample", Branch: a.config.Branch, Remote: a.config.Remote, PublishBranch: a.config.PublishBranch, Base: base, YAML: yaml, JSON: []byte(tt.input), OldYAMLHash: digest(yaml), OldJSONHash: digest(originalJSON), Phase: "prepared"}
			if tt.committed {
				if err = os.WriteFile(filepath.Join(a.config.Repository, "json/sample.json"), j.JSON, 0644); err != nil {
					t.Fatal(err)
				}
				testGit(t, a.config.Repository, "add", "json/sample.json")
				testGit(t, a.config.Repository, "commit", "-m", "invalid pending commit fixture")
				j.Commit = testGit(t, a.config.Repository, "rev-parse", "HEAD")
				j.Phase = "committed"
			}
			if err = a.save(j); err != nil {
				t.Fatal(err)
			}
			beforeYAML, beforeJSON, err := a.files("sample")
			if err != nil {
				t.Fatal(err)
			}
			beforeHead := testGit(t, a.config.Repository, "rev-parse", "HEAD")
			f := &faultRunner{base: a.runner}
			a.runner = f
			if _, err = runTool(t, a, "rules_resume", map[string]any{}); err == nil {
				t.Fatal("invalid pending rules accepted")
			}
			afterYAML, afterJSON, err := a.files("sample")
			if err != nil || !bytes.Equal(beforeYAML, afterYAML) || !bytes.Equal(beforeJSON, afterJSON) {
				t.Fatal("rejected recovery changed managed files")
			}
			if len(f.calls) != 0 || testGit(t, a.config.Repository, "rev-parse", "HEAD") != beforeHead || testGit(t, remote, "rev-parse", "dev") != base || testGit(t, remote, "rev-parse", "master") != base {
				t.Fatal("rejected recovery committed or pushed")
			}
		})
	}
}

func TestJSONEqualityRejectsDuplicateFields(t *testing.T) {
	a := []byte(`{"version":1,"rules":[{"domain":["example.com"]}]}`)
	b := []byte(`{"version":1,"rules":[{"domain":["other.example"],"domain":["example.com"]}]}`)
	if jsonEqual(a, b) {
		t.Fatal("invalid JSON bypassed sync validation")
	}
	// Ensure the direct recovery entry point also rejects missing JSON.
	app := &App{}
	if err := app.advance(context.Background(), &Journal{}); err == nil {
		t.Fatal("missing pending JSON accepted")
	}
}
