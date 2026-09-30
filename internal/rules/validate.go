package rules

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"unicode/utf8"
)

// ValidateJSON checks the version-1 headless rule subset managed by this project.
// References: sing-box rule-set/source-format and rule-set/headless-rule.
// This is not a validator for arbitrary sing-box configurations or rule types.
func ValidateJSON(data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("JSON file is missing or empty")
	}
	if !utf8.Valid(data) {
		return fmt.Errorf("JSON must be UTF-8")
	}
	if err := uniqueJSONFields(data); err != nil {
		return err
	}
	var source map[string]json.RawMessage
	if err := json.Unmarshal(data, &source); err != nil || source == nil {
		return fmt.Errorf("rule-set must be a JSON object")
	}
	for key := range source {
		if key != "version" && key != "rules" {
			return fmt.Errorf("rule-set contains a field outside the managed version-1 format")
		}
	}
	var version int
	if json.Unmarshal(source["version"], &version) != nil || version != 1 {
		return fmt.Errorf("version must be the integer 1 for this managed rule-set format")
	}
	var entries []json.RawMessage
	if json.Unmarshal(source["rules"], &entries) != nil || entries == nil {
		return fmt.Errorf("rules must be an array; use [] for an empty rule-set")
	}
	for i, entry := range entries {
		if err := validateHeadlessRule(entry); err != nil {
			return fmt.Errorf("rules[%d]: %w", i, err)
		}
	}
	return nil
}

func validateHeadlessRule(data []byte) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return fmt.Errorf("rule must be an object")
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	matched := false
	for _, key := range keys {
		raw := fields[key]
		switch key {
		case "type":
			var kind string
			if json.Unmarshal(raw, &kind) != nil || kind != "default" {
				return fmt.Errorf("type must be default; logical rules are outside the managed format")
			}
		case "invert":
			var invert bool
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &invert) != nil || invert {
				return fmt.Errorf("invert must be false in the managed format")
			}
		case "domain", "domain_suffix", "domain_keyword", "ip_cidr":
			values, err := stringValues(raw)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			for i, value := range values {
				if err := validateValue(key, value); err != nil {
					return fmt.Errorf("%s[%d]: %w", key, i, err)
				}
			}
			matched = true
		default:
			// Do not echo untrusted field names or values into logs/tool responses.
			return fmt.Errorf("rule contains a field outside domain/domain_suffix/domain_keyword/ip_cidr/type/invert")
		}
	}
	if !matched {
		return fmt.Errorf("rule has no match values; use rules: [] instead of a catch-all empty object")
	}
	return nil
}

func stringValues(raw []byte) ([]string, error) {
	var values []string
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fmt.Errorf("expected a string or non-empty string array")
		}
		values = []string{value}
	} else if json.Unmarshal(raw, &values) != nil || len(values) == 0 {
		return nil, fmt.Errorf("expected a string or non-empty string array")
	}
	return values, nil
}

func validateValue(field, value string) error {
	if field == "ip_cidr" {
		if _, err := netip.ParsePrefix(value); err == nil {
			return nil
		}
		if address, err := netip.ParseAddr(value); err == nil && address.Zone() == "" {
			return nil
		}
		return fmt.Errorf("expected an IPv4/IPv6 address or CIDR prefix")
	}
	kind := map[string]string{"domain": "DOMAIN", "domain_suffix": "DOMAIN-SUFFIX", "domain_keyword": "DOMAIN-KEYWORD"}[field]
	if rule, err := ParseRule(kind + "," + value); err != nil || rule.Value != value {
		return fmt.Errorf("invalid domain match value under the project's rule policy")
	}
	return nil
}

// encoding/json normally keeps the last duplicate object field. Reject it so
// an invalid earlier value cannot hide behind a later valid value during sync.
func uniqueJSONFields(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 8 {
			return fmt.Errorf("JSON nesting exceeds the managed rule-set format")
		}
		token, err := d.Token()
		if err != nil {
			return fmt.Errorf("invalid JSON syntax at byte %d", d.InputOffset())
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		seen := map[string]bool{}
		for d.More() {
			if delim == '{' {
				key, err := d.Token()
				if err != nil {
					return fmt.Errorf("invalid JSON object at byte %d", d.InputOffset())
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return fmt.Errorf("duplicate JSON object field at byte %d", d.InputOffset())
				}
				seen[name] = true
			}
			if err := walk(depth + 1); err != nil {
				return err
			}
		}
		if _, err := d.Token(); err != nil {
			return fmt.Errorf("invalid JSON syntax at byte %d", d.InputOffset())
		}
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON rule-set object")
	}
	return nil
}
