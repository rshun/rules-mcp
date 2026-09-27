package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path"
	"strings"

	"rules-mcp/internal/rules"
)

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func (a *App) syncFile(ctx context.Context, name string, data []byte) error {
	if !rules.NamePattern.MatchString(name) {
		return fmt.Errorf("invalid rule name")
	}
	o := a.config.OpenWrt
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("cannot create upload identifier")
	}
	dir := o.JSONDirectory
	destination := path.Join(dir, name+".json")
	temp := path.Join(dir, ".rules-mcp-"+hex.EncodeToString(nonce[:])+".tmp")
	// Only fixed shell syntax and quoted configuration-generated paths are used.
	// noclobber prevents symlink/temp collisions. SHA-256 is checked before rename
	// and after publication; failed uploads leave the previous JSON intact.
	script := "set -eu; umask 022; " +
		"test -d " + shellQuote(dir) + "; " +
		"test \"$(readlink -f " + shellQuote(dir) + ")\" = " + shellQuote(dir) + "; " +
		"test ! -L " + shellQuote(destination) + "; " +
		"if test -e " + shellQuote(destination) + "; then test -f " + shellQuote(destination) + "; fi; " +
		"(set -C; cat > " + shellQuote(temp) + "); " +
		"printf '%s  %s\\n' " + shellQuote(digest(data)) + " " + shellQuote(temp) + " | sha256sum -c - >/dev/null; " +
		"mv -f " + shellQuote(temp) + " " + shellQuote(destination) + "; " +
		"printf '%s  %s\\n' " + shellQuote(digest(data)) + " " + shellQuote(destination) + " | sha256sum -c - >/dev/null"
	args := []string{"-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "IdentitiesOnly=yes", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=2", "-o", "UserKnownHostsFile=" + o.KnownHostsFile, "-i", o.IdentityFile, "-p", fmt.Sprint(o.Port), o.User + "@" + o.Host, script}
	if _, err := a.runner.Run(ctx, "ssh", args, data); err != nil {
		return fmt.Errorf("OpenWrt JSON upload or hash verification failed: %w", err)
	}
	return nil
}

func (a *App) verifyPublished(ctx context.Context, head string) error {
	if err := a.fetch(ctx); err != nil {
		return err
	}
	branches := []string{a.config.Branch}
	if a.config.PublishBranch != "" {
		branches = append(branches, a.config.PublishBranch)
	}
	for _, branch := range branches {
		remote, err := a.git(ctx, "rev-parse", "refs/remotes/"+a.config.Remote+"/"+branch)
		if err != nil || remote != head {
			return fmt.Errorf("local HEAD and remote branches differ; synchronization refused")
		}
	}
	return nil
}

func (a *App) syncAll(ctx context.Context) (any, error) {
	if !a.config.OpenWrt.Enabled {
		return nil, fmt.Errorf("OpenWrt synchronization is disabled")
	}
	j, err := a.journal()
	if err != nil {
		return nil, err
	}
	if j != nil && j.Phase != "complete" {
		return nil, fmt.Errorf("resume the pending operation first")
	}
	if err = a.clean(ctx); err != nil {
		return nil, err
	}
	head, err := a.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	if err = a.verifyPublished(ctx, head); err != nil {
		return nil, err
	}
	listing, err := a.list()
	if err != nil {
		return nil, err
	}
	names := listing.(map[string]any)["names"].([]string)
	files := map[string][]byte{}
	// Validate every file before publishing the first file.
	for _, name := range names {
		y, js, e := a.files(name)
		if e != nil {
			return nil, e
		}
		d, e := rules.Parse(y)
		if e != nil {
			return nil, fmt.Errorf("%s: %w", name, e)
		}
		generated, e := d.JSON()
		if e != nil {
			return nil, e
		}
		if !jsonEqual(generated, js) {
			return nil, fmt.Errorf("%s: JSON differs from YAML; preview/apply an empty edit first", name)
		}
		files[name] = js
	}
	synced := []string{}
	for _, name := range names {
		if err = a.syncFile(ctx, name, files[name]); err != nil {
			return map[string]any{"synced": synced, "failed": name, "commit": head}, err
		}
		synced = append(synced, name)
	}
	return map[string]any{"synced": synced, "commit": head}, nil
}
