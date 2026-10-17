// Command notices writes web/public/third-party-notices.txt: the licence of
// every library built into Homestack, in its binary and its web UI, as those
// licences ask (MIT, ISC and BSD notices go with every copy). The web build
// copies the file into the UI, so the binary carries it and serves it at
// /third-party-notices.txt.
//
//	go run ./scripts/notices          rewrite it (after changing dependencies)
//	go run ./scripts/notices -check   fail if it's out of date (scripts/check.sh)
//
// Run it from the repository root, with web/node_modules installed (npm ci).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const out = "web/public/third-party-notices.txt"

type pkg struct{ name, version, licence, source, text string }

// extra: licence texts a package keeps in other files and says apply to it.
var extra = map[string][]string{
	// noVNC: MPL-2.0 for its code, BSD for parts of it, MIT for pako (vendor/).
	"@novnc/novnc": {"docs/LICENSE.MPL-2.0", "docs/LICENSE.BSD-3-Clause", "docs/LICENSE.BSD-2-Clause", "vendor/pako/LICENSE"},
}

func main() {
	check := flag.Bool("check", false, "fail if "+out+" is out of date")
	flag.Parse()
	text, err := notices()
	if err != nil {
		fmt.Fprintln(os.Stderr, "notices:", err)
		os.Exit(1)
	}
	if *check {
		if old, _ := os.ReadFile(out); string(old) != text {
			fmt.Fprintf(os.Stderr, "%s is out of date. Run: go run ./scripts/notices\n", out)
			os.Exit(1)
		}
		return
	}
	if err := os.WriteFile(out, []byte(text), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "notices:", err)
		os.Exit(1)
	}
}

func notices() (string, error) {
	goPkgs, err := goModules()
	if err != nil {
		return "", err
	}
	webPkgs, err := npmPackages()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(`Third-party software in Homestack
=================================

Homestack is free software under the GNU Affero General Public License,
version 3 or later (LICENSE, in its source code). It's built with the
software below, which keeps its own licence. Each one's licence follows.

Written by scripts/notices from go.mod and web/package-lock.json.
`)
	for _, p := range append(goPkgs, webPkgs...) {
		title := p.name
		if p.version != "" {
			title += " " + p.version
		}
		if p.licence != "" {
			title += " (" + p.licence + ")"
		}
		rule := strings.Repeat("=", 72)
		fmt.Fprintf(&b, "\n\n%s\n%s\nSource: %s\n%s\n\n%s\n", rule, title, p.source, rule, p.text)
	}
	return b.String(), nil
}

// goModules: Go's standard library, and every module the binary is built
// from (for Linux, which is all it builds for).
func goModules() ([]pkg, error) {
	root, err := goCmd("env", "GOROOT")
	if err != nil {
		return nil, err
	}
	std, err := os.ReadFile(filepath.Join(strings.TrimSpace(root), "LICENSE"))
	if err != nil {
		return nil, fmt.Errorf("Go's own licence: %w", err)
	}
	out := []pkg{{name: "Go standard library and runtime", licence: "BSD-3-Clause", source: "https://go.dev/dl/", text: tidy(string(std))}}

	list, err := goCmd("list", "-deps", "-f", "{{with .Module}}{{if not .Main}}{{.Path}}\t{{.Version}}\t{{.Dir}}{{end}}{{end}}", ".")
	if err != nil {
		return nil, err
	}
	var mods []pkg
	seen := map[string]bool{}
	for l := range strings.Lines(list) {
		f := strings.Split(strings.TrimSpace(l), "\t")
		if len(f) != 3 || seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		text, err := licenceText(f[2])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f[0], err)
		}
		mods = append(mods, pkg{name: f[0], version: f[1], licence: guess(text), source: "https://pkg.go.dev/" + f[0] + "@" + f[1], text: text})
	}
	slices.SortFunc(mods, func(a, b pkg) int { return strings.Compare(a.name, b.name) })
	return append(out, mods...), nil
}

func goCmd(args ...string) (string, error) {
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64")
	b, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go %s: %w", strings.Join(args, " "), err)
	}
	return string(b), nil
}

// npmPackages: every package the web UI ships (the lockfile's packages that
// aren't only for development).
func npmPackages() ([]pkg, error) {
	b, err := os.ReadFile("web/package-lock.json")
	if err != nil {
		return nil, err
	}
	var lock struct {
		Packages map[string]struct {
			Version     string `json:"version"`
			License     string `json:"license"`
			Dev         bool   `json:"dev"`
			DevOptional bool   `json:"devOptional"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(b, &lock); err != nil {
		return nil, fmt.Errorf("web/package-lock.json: %w", err)
	}
	var out []pkg
	seen := map[string]bool{}
	for path, p := range lock.Packages {
		if path == "" || p.Dev || p.DevOptional {
			continue
		}
		name := path[strings.LastIndex(path, "node_modules/")+len("node_modules/"):]
		if seen[name+"@"+p.Version] {
			continue
		}
		seen[name+"@"+p.Version] = true
		dir := filepath.Join("web", filepath.FromSlash(path))
		text, err := licenceText(dir)
		if err != nil {
			return nil, fmt.Errorf("%s (is web/node_modules installed? cd web && npm ci): %w", name, err)
		}
		for _, f := range extra[name] {
			b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f)))
			if err != nil {
				return nil, fmt.Errorf("%s: %w (it moved? update extra in scripts/notices)", name, err)
			}
			text += "\n\n--- " + f + " ---\n\n" + tidy(string(b))
		}
		out = append(out, pkg{name: name, version: p.Version, licence: p.License, source: npmSource(dir, name, p.Version), text: text})
	}
	slices.SortFunc(out, func(a, b pkg) int { return strings.Compare(a.name+"@"+a.version, b.name+"@"+b.version) })
	return out, nil
}

// npmSource is where a package's source is: its repository, as its
// package.json gives it, else its page on npm.
func npmSource(dir, name, version string) string {
	var m struct {
		Repository json.RawMessage `json:"repository"`
	}
	if b, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil && json.Unmarshal(b, &m) == nil {
		var url string
		var obj struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(m.Repository, &url) != nil && json.Unmarshal(m.Repository, &obj) == nil {
			url = obj.URL
		}
		url = strings.TrimSuffix(strings.TrimPrefix(url, "git+"), ".git")
		switch {
		case strings.HasPrefix(url, "github:"):
			url = "https://github.com/" + strings.TrimPrefix(url, "github:")
		case strings.HasPrefix(url, "git://"):
			url = "https://" + strings.TrimPrefix(url, "git://")
		case url != "" && !strings.Contains(url, "://"): // "owner/repo"
			url = "https://github.com/" + url
		}
		if strings.HasPrefix(url, "https://") {
			return url
		}
	}
	return "https://www.npmjs.com/package/" + name + "/v/" + version
}

var reLicenceFile = regexp.MustCompile(`(?i)^(licen[cs]e|copying)([.-].*)?$`)

// licenceText is the licence file at the top of a package's folder.
func licenceText(dir string) (string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range ents {
		if !e.IsDir() && reLicenceFile.MatchString(e.Name()) {
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				return "", err
			}
			return tidy(string(b)), nil
		}
	}
	return "", fmt.Errorf("no licence file in %s", dir)
}

// tidy makes a licence text the same on every machine (Windows line
// endings, trailing spaces).
func tidy(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var lines []string
	for l := range strings.Lines(s) {
		lines = append(lines, strings.TrimRight(l, " \t\r\n"))
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

// guess names a Go module's licence from its text (Go modules don't say).
func guess(text string) string {
	switch {
	case strings.Contains(text, "Permission to use, copy, modify, and/or distribute") ||
		strings.Contains(text, "Permission to use, copy, modify, and distribute this software for any"):
		return "ISC"
	case strings.Contains(text, "Permission is hereby granted, free of charge"):
		return "MIT"
	case strings.Contains(text, "Redistribution and use in source and binary forms"):
		return "BSD"
	case strings.Contains(text, "Apache License"):
		return "Apache-2.0"
	case strings.Contains(text, "Mozilla Public License"):
		return "MPL-2.0"
	}
	return ""
}
