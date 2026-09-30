package rules

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditPreservesCommentsAndConvertsAllTypes(t *testing.T) {
	input := "payload:\r\n# 中文说明\r\n  - DOMAIN,example.com # keep\r\n  - DOMAIN-SUFFIX,example.org\r\n  - DOMAIN-KEYWORD,example\r\n  - IP-CIDR,192.0.2.7/24,no-resolve\r\n  - IP-CIDR6,2001:db8::1/32,no-resolve\r\n  - # empty comment\r\n"
	d, err := Parse([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Warnings) != 1 {
		t.Fatal("missing empty-item warning")
	}
	out, err := d.Apply(Edit{Add: []string{"DOMAIN,new.example", "DOMAIN,new.example"}, Remove: []string{"DOMAIN-SUFFIX,example.org"}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("# 中文说明\r\n")) || !bytes.Contains(out, []byte("# keep")) || bytes.Count(out, []byte("DOMAIN,new.example")) != 1 || bytes.Contains(out, []byte("example.org")) {
		t.Fatal("incorrect edit")
	}
	updated, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	b, err := updated.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var source Source
	if err = json.Unmarshal(b, &source); err != nil {
		t.Fatal(err)
	}
	if source.Version != 1 || len(source.Rules) != 1 || len(source.Rules[0]["domain"]) != 2 || len(source.Rules[0]["ip_cidr"]) != 2 {
		t.Fatalf("incorrect conversion: %s", b)
	}
	if bytes.Contains(b, []byte("no-resolve")) {
		t.Fatal("Clash-only flag leaked")
	}
}

func TestUnsafeAndUnsupportedRulesFail(t *testing.T) {
	for _, s := range []string{"MATCH,all", "DOMAIN,example.com,DIRECT", "IP-CIDR,::1/128", "IP-CIDR6,192.0.2.0/24", "DOMAIN,example.com\npayload:", "DOMAIN,https://example.com", "DOMAIN,example.com,no-resolve", "DOMAIN,*.example.com"} {
		if _, err := ParseRule(s); err == nil {
			t.Fatalf("accepted %q", s)
		}
	}
	for _, s := range []string{"payload: []", "payload:\nother: value", "payload:\n  - &anchor something", "payload:\n  - DOMAIN,example.com\npayload:", "payload:\n  - \"DOMAIN,example.com\" junk"} {
		if _, err := Parse([]byte(s)); err == nil {
			t.Fatalf("accepted unsupported YAML: %q", s)
		}
	}
}

func TestEmptyDoesNotGenerateCatchAll(t *testing.T) {
	d, err := Parse([]byte("payload:\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"rules": []`)) {
		t.Fatalf("unsafe empty rules: %s", b)
	}
}
func TestQuotedRulesAndRemovalValidation(t *testing.T) {
	d, err := Parse([]byte("payload:\n  - 'DOMAIN,one.example'\n  - \"DOMAIN,two.example\" # comment\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Rules) != 2 {
		t.Fatal("quoted rules not parsed")
	}
	if _, err = d.Apply(Edit{Remove: []string{"DOMAIN,missing.example"}}); err == nil {
		t.Fatal("missing removal accepted")
	}
	if _, err = d.Apply(Edit{Add: []string{"DOMAIN,one.example"}, Remove: []string{"DOMAIN,one.example"}}); err == nil {
		t.Fatal("contradictory edit accepted")
	}
}

func TestExistingCorpusReadOnly(t *testing.T) {
	dir := os.Getenv("RULES_MCP_TEST_CORPUS")
	if dir == "" {
		t.Skip("set RULES_MCP_TEST_CORPUS for read-only corpus validation")
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatal("no corpus")
	}
	count, warnings, blocked, invalidJSON := 0, 0, 0, 0
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		d, err := Parse(b)
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(file), err)
		}
		jsonName := strings.TrimSuffix(filepath.Base(file), ".yaml") + ".json"
		existing, readErr := os.ReadFile(filepath.Join(dir, "json", jsonName))
		if readErr != nil && !os.IsNotExist(readErr) {
			t.Fatal(readErr)
		}
		if validationErr := ValidateJSON(existing); validationErr != nil {
			invalidJSON++
			t.Logf("existing JSON validation failed for %s: %v", jsonName, validationErr)
		}
		generated, err := d.JSON()
		if err != nil {
			blocked++
			t.Logf("conversion blocked for %s: %v", filepath.Base(file), err)
			count += len(d.Rules)
			warnings += len(d.Warnings)
			continue
		}
		var value any
		if json.Unmarshal(generated, &value) != nil {
			t.Fatal("invalid JSON")
		}
		if strings.Contains(string(generated), "no-resolve") {
			t.Fatal("unsupported JSON flag")
		}
		count += len(d.Rules)
		warnings += len(d.Warnings)
	}
	t.Logf("read-only corpus: %d files, %d rules, %d warnings, %d files blocked by conversion, %d existing JSON files missing or invalid", len(files), count, warnings, blocked, invalidJSON)
}

func TestCanRemoveInvalidLegacyDomainWithoutPublishingIt(t *testing.T) {
	d, err := Parse([]byte("payload:\n  - DOMAIN-SUFFIX,example.com/v1\n  - DOMAIN,valid.example\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.JSON(); err == nil {
		t.Fatal("invalid legacy domain was converted")
	}
	b, err := d.Apply(Edit{Remove: []string{"DOMAIN-SUFFIX,example.com/v1"}})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = updated.JSON(); err != nil {
		t.Fatal(err)
	}
}
