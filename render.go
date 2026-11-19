package main

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
)

//go:embed templates/haproxy.cfg.tmpl
var defaultTemplate string

type templateData struct {
	ACLs         []*ACL
	Services     []*Service
	TemplatePath string
}

// loadTemplate reads the template from dir, falling back to the embedded default.
func loadTemplate(dir string) (src, path string, err error) {
	path = filepath.Join(dir, templateFile)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return defaultTemplate, "(built-in default)", nil
	}
	if err != nil {
		return "", "", err
	}
	return string(b), path, nil
}

func render(st *State, tmplSrc, tmplPath string) ([]byte, error) {
	t, err := template.New("haproxy").Option("missingkey=error").Funcs(template.FuncMap{
		"join": strings.Join,
	}).Parse(tmplSrc)
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}
	st.sort()
	// Templates see .Hosts as the full hostname list (--host + expanded --sub).
	view := make([]*Service, len(st.Services))
	for i, s := range st.Services {
		hosts, err := st.AllHosts(s)
		if err != nil {
			return nil, err
		}
		c := *s
		c.Hosts = hosts
		view[i] = &c
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, templateData{ACLs: st.ACLs, Services: view, TemplatePath: tmplPath}); err != nil {
		return nil, fmt.Errorf("render template: %w", err)
	}
	return buf.Bytes(), nil
}

// apply renders the config, validates a candidate copy, installs it atomically
// and runs the reload command. It returns whether the installed file changed.
func apply(dir string, st *State, dryRun, force bool) (bool, error) {
	src, tpath, err := loadTemplate(dir)
	if err != nil {
		return false, err
	}
	out, err := render(st, src, tpath)
	if err != nil {
		return false, err
	}
	dst := st.Settings.OutputPath
	if !filepath.IsAbs(dst) {
		dst = filepath.Join(dir, dst)
	}
	if old, err := os.ReadFile(dst); err == nil && bytes.Equal(old, out) && !force {
		return false, nil
	}
	if dryRun {
		return true, nil
	}

	// Candidate lives next to the destination so the final rename is atomic
	// and relative paths inside the config behave the same.
	cand, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".candidate.*")
	if err != nil {
		return false, err
	}
	defer os.Remove(cand.Name())
	if _, err := cand.Write(out); err != nil {
		cand.Close()
		return false, err
	}
	if err := cand.Chmod(0o644); err != nil {
		cand.Close()
		return false, err
	}
	if err := cand.Close(); err != nil {
		return false, err
	}

	if c := st.Settings.ValidateCmd; c != "" {
		if err := runShell(strings.ReplaceAll(c, "{file}", shellQuote(cand.Name()))); err != nil {
			return false, fmt.Errorf("validation failed, %s left untouched: %w", dst, err)
		}
	}
	if err := os.Rename(cand.Name(), dst); err != nil {
		return false, err
	}
	if c := st.Settings.ReloadCmd; c != "" {
		if err := runShell(strings.ReplaceAll(c, "{file}", shellQuote(dst))); err != nil {
			return true, fmt.Errorf("config written to %s but reload failed: %w", dst, err)
		}
	}
	return true, nil
}

func runShell(cmd string) error {
	c := exec.Command("sh", "-c", cmd)
	c.Stdout = os.Stderr
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("`%s`: %w", cmd, err)
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
