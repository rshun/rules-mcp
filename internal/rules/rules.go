// Package rules edits the deliberately narrow Clash payload format used by this
// repository. It is not a general YAML parser: unsupported syntax is rejected.
package rules

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var NamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

type Rule struct {
	Type      string
	Value     string
	NoResolve bool
}

func (r Rule) String() string {
	s := r.Type + "," + r.Value
	if r.NoResolve {
		s += ",no-resolve"
	}
	return s
}

func ParseRule(s string) (Rule, error) {
	return parseRule(s, true)
}

func parseRule(s string, strictDomain bool) (Rule, error) {
	p := strings.Split(s, ",")
	if len(p) < 2 || len(p) > 3 {
		return Rule{}, fmt.Errorf("rule must have type,value[,no-resolve]")
	}
	r := Rule{Type: strings.TrimSpace(p[0]), Value: strings.TrimSpace(p[1])}
	if r.Value == "" || strings.ContainsAny(r.Value, "\"'\\#[]{}&*!|><`\x00") || strings.ContainsFunc(r.Value, unicode.IsSpace) || strings.ContainsFunc(r.Value, unicode.IsControl) {
		return Rule{}, fmt.Errorf("invalid rule value")
	}
	isIP := r.Type == "IP-CIDR" || r.Type == "IP-CIDR6"
	if len(p) == 3 {
		if !isIP || strings.TrimSpace(p[2]) != "no-resolve" {
			return Rule{}, fmt.Errorf("unsupported rule option")
		}
		r.NoResolve = true
	}
	switch r.Type {
	case "DOMAIN", "DOMAIN-SUFFIX", "DOMAIN-KEYWORD":
		if strictDomain && strings.ContainsAny(r.Value, ":/=@?;") {
			return Rule{}, fmt.Errorf("invalid domain rule")
		}
	case "IP-CIDR", "IP-CIDR6":
		p, err := netip.ParsePrefix(r.Value)
		if err != nil || (r.Type == "IP-CIDR" && !p.Addr().Is4()) || (r.Type == "IP-CIDR6" && !p.Addr().Is6()) {
			return Rule{}, fmt.Errorf("invalid CIDR or address family")
		}
	default:
		return Rule{}, fmt.Errorf("unsupported rule type")
	}
	return r, nil
}

type Line struct {
	Raw  string
	Rule *Rule
}
type Document struct {
	Lines    []Line
	Rules    []Rule
	Newline  string
	Warnings []string
}

func Parse(data []byte) (Document, error) {
	d := Document{Newline: "\n", Warnings: []string{}}
	if !utf8.Valid(data) {
		return d, fmt.Errorf("YAML must be UTF-8")
	}
	if bytes.Contains(data, []byte("\r\n")) {
		d.Newline = "\r\n"
	}
	s := strings.TrimPrefix(string(data), "\ufeff")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if strings.ContainsAny(s, "\r\x00\t") {
		return d, fmt.Errorf("unsupported control character in YAML")
	}
	header := false
	for i, raw := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		line := Line{Raw: raw}
		trim := strings.TrimSpace(raw)
		switch {
		case trim == "" || strings.HasPrefix(trim, "#"):
		case trim == "payload:" && !header && raw == trim:
			header = true
		case header && strings.HasPrefix(trim, "- "):
			if !strings.HasPrefix(raw, "  - ") {
				return d, fmt.Errorf("line %d: sequence entries must use exactly two spaces of indentation", i+1)
			}
			item := strings.TrimSpace(strings.TrimPrefix(trim, "- "))
			if strings.HasPrefix(item, "#") {
				d.Warnings = append(d.Warnings, fmt.Sprintf("line %d: empty comment item ignored", i+1))
				break
			}
			// Preserve the original line while parsing optional inline comments.
			if strings.HasPrefix(item, "\"") || strings.HasPrefix(item, "'") {
				quote := item[0]
				end := strings.IndexByte(item[1:], quote)
				if end < 0 {
					return d, fmt.Errorf("line %d: unterminated quoted rule", i+1)
				}
				end++
				tail := strings.TrimSpace(item[end+1:])
				if tail != "" && !strings.HasPrefix(tail, "#") {
					return d, fmt.Errorf("line %d: unsupported YAML", i+1)
				}
				item = item[1:end]
			} else if at := strings.Index(item, " #"); at >= 0 {
				item = strings.TrimSpace(item[:at])
			}
			r, err := parseRule(item, false)
			if err != nil {
				return d, fmt.Errorf("line %d: %w", i+1, err)
			}
			line.Rule = &r
			if _, err := ParseRule(item); err != nil {
				d.Warnings = append(d.Warnings, fmt.Sprintf("line %d: invalid domain rule; remove or replace before JSON generation", i+1))
			}
			d.Rules = append(d.Rules, r)
		default:
			return d, fmt.Errorf("line %d: unsupported YAML; expected payload sequence", i+1)
		}
		d.Lines = append(d.Lines, line)
	}
	if !header {
		return d, fmt.Errorf("missing payload header")
	}
	return d, nil
}

type Source struct {
	Version int                   `json:"version"`
	Rules   []map[string][]string `json:"rules"`
}

func (d Document) JSON() ([]byte, error) {
	for i, line := range d.Lines {
		if line.Rule != nil {
			if _, err := ParseRule(line.Rule.String()); err != nil {
				return nil, fmt.Errorf("line %d: %w; remove or replace this rule", i+1, err)
			}
		}
	}
	fields := map[string][]string{}
	seen := map[string]bool{}
	for _, r := range d.Rules {
		key := map[string]string{"DOMAIN": "domain", "DOMAIN-SUFFIX": "domain_suffix", "DOMAIN-KEYWORD": "domain_keyword", "IP-CIDR": "ip_cidr", "IP-CIDR6": "ip_cidr"}[r.Type]
		value := r.Value
		// Keep host bits for compatibility with the existing source files.
		if !seen[key+"\x00"+value] {
			fields[key] = append(fields[key], value)
			seen[key+"\x00"+value] = true
		}
	}
	list := []map[string][]string{}
	// An empty object is a catch-all rule; an empty rules array matches nothing.
	if len(fields) > 0 {
		list = append(list, fields)
	}
	b, err := json.MarshalIndent(Source{Version: 1, Rules: list}, "", "    ")
	return append(b, '\n'), err
}

type Edit struct {
	Add    []string `json:"add"`
	Remove []string `json:"remove"`
}

func (d Document) Apply(edit Edit) ([]byte, error) {
	remove := map[string]bool{}
	existing := map[string]bool{}
	for _, r := range d.Rules {
		existing[r.String()] = true
	}
	for _, s := range edit.Remove {
		r, err := parseRule(s, false)
		if err != nil {
			return nil, err
		}
		if !existing[r.String()] {
			return nil, fmt.Errorf("a rule requested for removal does not exist")
		}
		remove[r.String()] = true
	}
	var added []Rule
	for _, s := range edit.Add {
		r, err := ParseRule(s)
		if err != nil {
			return nil, err
		}
		if remove[r.String()] {
			return nil, fmt.Errorf("the same rule cannot be added and removed")
		}
		if !existing[r.String()] {
			added = append(added, r)
			existing[r.String()] = true
		}
	}
	var out []string
	for _, line := range d.Lines {
		if line.Rule != nil && remove[line.Rule.String()] {
			continue
		}
		out = append(out, line.Raw)
	}
	for _, r := range added {
		out = append(out, "  - "+r.String())
	}
	return []byte(strings.Join(out, d.Newline) + d.Newline), nil
}

func Revision(yaml, js []byte) string {
	h := sha256.New()
	h.Write(yaml)
	h.Write([]byte{0})
	h.Write(js)
	return hex.EncodeToString(h.Sum(nil))
}
