package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Config struct {
	Listen         string `json:"listen"`
	Repository     string `json:"repository"`
	Branch         string `json:"branch"`
	Remote         string `json:"remote"`
	PublishBranch  string `json:"publish_branch"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

func Decode(data []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return fmt.Errorf("invalid JSON fields or values")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("expected one JSON object")
	}
	return nil
}

func LoadConfig(name string) (Config, error) {
	c := Config{Listen: "127.0.0.1:8787", Remote: "origin", TimeoutSeconds: 120}
	b, err := os.ReadFile(name)
	if err != nil {
		return c, fmt.Errorf("cannot read configuration")
	}
	if err = Decode(b, &c); err != nil {
		return c, err
	}
	return c, c.Validate()
}

var refPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_/-]*$`)

func (c Config) Validate() error {
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || port == "" {
		return fmt.Errorf("unauthenticated HTTP must bind to a loopback IP and port")
	}
	if !filepath.IsAbs(c.Repository) {
		return fmt.Errorf("repository must be an absolute path")
	}
	for _, ref := range []string{c.Branch, c.Remote, c.PublishBranch} {
		if ref == "" && ref == c.PublishBranch {
			continue
		}
		if !refPattern.MatchString(ref) || strings.Contains(ref, "//") || strings.HasSuffix(ref, "/") {
			return fmt.Errorf("invalid Git branch or remote name")
		}
	}
	if c.Branch == "" || c.Remote == "" || c.PublishBranch == c.Branch {
		return fmt.Errorf("branch and remote must be non-empty; publish_branch must differ (use an empty publish_branch for a single branch)")
	}
	if c.TimeoutSeconds < 1 || c.TimeoutSeconds > 600 {
		return fmt.Errorf("timeout_seconds must be between 1 and 600")
	}
	return nil
}
