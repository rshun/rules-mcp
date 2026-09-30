package rules

import (
	"strings"
	"testing"
)

func TestValidateJSONManagedSingBoxRules(t *testing.T) {
	for _, input := range []string{
		`{"version":1,"rules":[]}`,
		`{"version":1,"rules":[{"domain":["example.com"],"domain_suffix":[".example.org"],"domain_keyword":["example"],"ip_cidr":["192.0.2.0/24","2001:db8::/32"]}]}`,
		`{"version":1,"rules":[{"type":"default","invert":false,"domain":"example.com"},{"ip_cidr":["192.0.2.1","2001:db8::1","192.0.2.129/24"]}]}`,
		`{"version":1,"rules":[{"domain":["example.com","example.com","_service.example","例子.测试"]}]}`,
	} {
		if err := ValidateJSON([]byte(input)); err != nil {
			t.Fatalf("valid managed rule-set rejected: %v", err)
		}
	}
}

func TestValidateJSONRejectsBrokenOrUnsupportedRules(t *testing.T) {
	tests := []struct {
		name, input, message string
	}{
		{"missing", "", "missing or empty"},
		{"syntax", `{"version":1,"rules":[`, "JSON syntax"},
		{"trailing", `{"version":1,"rules":[]} {}`, "exactly one"},
		{"null", `null`, "object"},
		{"array", `[]`, "object"},
		{"missing_version", `{"rules":[]}`, "integer 1"},
		{"wrong_version", `{"version":2,"rules":[]}`, "integer 1"},
		{"string_version", `{"version":"1","rules":[]}`, "integer 1"},
		{"missing_rules", `{"version":1}`, "rules must be an array"},
		{"null_rules", `{"version":1,"rules":null}`, "rules must be an array"},
		{"unknown_source_field", `{"version":1,"rules":[],"outbound":"direct"}`, "outside the managed"},
		{"empty_rule", `{"version":1,"rules":[{}]}`, "no match values"},
		{"null_rule", `{"version":1,"rules":[null]}`, "rule must be an object"},
		{"unknown_rule_field", `{"version":1,"rules":[{"domain_suffix_typo":["example.com"]}]}`, "outside domain"},
		{"route_action", `{"version":1,"rules":[{"domain":["example.com"],"action":"route"}]}`, "outside domain"},
		{"logical", `{"version":1,"rules":[{"type":"logical","domain":"example.com"}]}`, "type must be default"},
		{"inverted", `{"version":1,"rules":[{"invert":true,"domain":"example.com"}]}`, "invert must be false"},
		{"null_invert", `{"version":1,"rules":[{"invert":null,"domain":"example.com"}]}`, "invert must be false"},
		{"empty_array", `{"version":1,"rules":[{"domain":[]}]}`, "non-empty string array"},
		{"wrong_item_type", `{"version":1,"rules":[{"domain":[123]}]}`, "non-empty string array"},
		{"null_value", `{"version":1,"rules":[{"domain":null}]}`, "non-empty string array"},
		{"empty_item", `{"version":1,"rules":[{"domain":["example.com",""]}]}`, "domain[1]"},
		{"null_item", `{"version":1,"rules":[{"domain":[null]}]}`, "domain[0]"},
		{"surrounding_space", `{"version":1,"rules":[{"domain":[" example.com "]}]}`, "domain[0]"},
		{"url_suffix", `{"version":1,"rules":[{"domain_suffix":["example.com/path"]}]}`, "domain_suffix[0]"},
		{"bad_cidr", `{"version":1,"rules":[{"ip_cidr":["192.0.2.0/33"]}]}`, "ip_cidr[0]"},
		{"bad_ipv6", `{"version":1,"rules":[{"ip_cidr":["2001:db8::/129"]}]}`, "ip_cidr[0]"},
		{"zoned_address", `{"version":1,"rules":[{"ip_cidr":["fe80::1%eth0"]}]}`, "ip_cidr[0]"},
		{"duplicate_source_key", `{"version":2,"version":1,"rules":[]}`, "duplicate JSON object field"},
		{"duplicate_rule_key", `{"version":1,"rules":[{"ip_cidr":["bad"],"ip_cidr":["192.0.2.0/24"]}]}`, "duplicate JSON object field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateJSON([]byte(tt.input))
			if err == nil || !strings.Contains(err.Error(), tt.message) {
				t.Fatalf("expected %q, got %v", tt.message, err)
			}
		})
	}
	if err := ValidateJSON([]byte{'{', '"', 0xff, '"', ':', '1', '}'}); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}

func TestValidationErrorsDoNotEchoUntrustedValues(t *testing.T) {
	for _, input := range []string{
		`{"version":1,"rules":[{"domain":["https://private-value.invalid/path"]}]}`,
		`{"version":1,"rules":[{"private-value.invalid":[]}]}`,
		`{"version":1,"rules":[{"ip_cidr":["private-value.invalid"]}]}`,
	} {
		err := ValidateJSON([]byte(input))
		if err == nil || strings.Contains(err.Error(), "private-value.invalid") {
			t.Fatal("untrusted rule value was accepted or echoed in the error")
		}
	}
}
