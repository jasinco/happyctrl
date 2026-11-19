package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestState(t *testing.T) (string, *State) {
	t.Helper()
	dir := t.TempDir()
	st := defaultState()
	st.Settings.OutputPath = "haproxy.cfg"
	st.Settings.ValidateCmd = "grep -q 'backend be_' {file}"
	st.Settings.ReloadCmd = ""
	return dir, st
}

func TestAddValidation(t *testing.T) {
	_, st := newTestState(t)
	ok := &Service{Name: "jf", Hosts: []string{"JF.Home.LAN"}, Servers: []Server{{Addr: "127.0.0.1:8096"}}}
	if err := st.Upsert(ok, false); err != nil {
		t.Fatal(err)
	}
	if st.Services[0].Hosts[0] != "jf.home.lan" || st.Services[0].Servers[0].Name != "s1" {
		t.Fatalf("not normalized: %+v", st.Services[0])
	}
	bad := []*Service{
		{Name: "jf", Hosts: []string{"x.lan"}, Servers: []Server{{Addr: "10.0.0.1:1"}}},      // exists
		{Name: "x", Hosts: []string{"jf.home.lan"}, Servers: []Server{{Addr: "10.0.0.1:1"}}}, // host conflict
		{Name: "x", Hosts: []string{"x.lan"}, Servers: []Server{{Addr: "noport"}}},
		{Name: "x", Hosts: []string{"x.lan"}, Servers: []Server{{Addr: "jellyfin:8096"}}},                       // hostname, not IP
		{Name: "x", Hosts: []string{"x.lan"}, Servers: []Server{{Addr: "10.0.0.1:0"}}},                          // bad port
		{Name: "x", Hosts: []string{"x.lan"}, Servers: []Server{{Addr: "10.0.0.1:70000"}}},                      // bad addr
		{Name: "x y", Hosts: []string{"x.lan"}, Servers: []Server{{Addr: "10.0.0.1:1"}}},                        // bad name
		{Name: "x", Hosts: []string{"x.lan\nfoo"}, Servers: []Server{{Addr: "10.0.0.1:1"}}},                     // injection
		{Name: "x", Hosts: []string{"x.lan"}, Servers: []Server{{Addr: "10.0.0.1:1"}}, Extra: []string{"a\nb"}}, // injection
		{Name: "notfound", Hosts: []string{"x.lan"}, Servers: []Server{{Addr: "10.0.0.1:1"}}},                   // reserved
	}
	for i, s := range bad {
		if err := st.Upsert(s, false); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
	if err := st.Upsert(&Service{Name: "v6", Hosts: []string{"v6.lan"}, Servers: []Server{{Addr: "[::1]:8080"}}}, false); err != nil {
		t.Errorf("ipv6 server rejected: %v", err)
	}
	st.Remove("v6")
	if len(st.Services) != 1 {
		t.Fatalf("failed upserts mutated state: %d services", len(st.Services))
	}
}

func TestRender(t *testing.T) {
	_, st := newTestState(t)
	st.Upsert(&Service{Name: "web", Hosts: []string{"a.lan", "b.lan"}, HealthCheck: "/hz",
		Servers: []Server{{Addr: "127.0.0.1:81"}, {Addr: "127.0.0.1:82"}}}, false)
	st.Upsert(&Service{Name: "nas", Hosts: []string{"nas.lan"}, ServerSSL: true, Servers: []Server{{Addr: "10.0.0.5:5001"}}}, false)
	out, err := render(st, defaultTemplate, "test")
	if err != nil {
		t.Fatal(err)
	}
	s := normIndent(string(out))
	for _, want := range []string{
		"use_backend be_web if { var(txn.host) -m str a.lan b.lan }",
		"use_backend be_nas if { var(txn.host) -m str nas.lan }",
		"server s1 127.0.0.1:81 check",
		"server s2 127.0.0.1:82 check",
		"http-check send meth GET uri /hz",
		"server s1 10.0.0.5:5001 check ssl verify none",
		"default_backend be_notfound",
		`http-after-response set-header Strict-Transport-Security "max-age=63072000"`,
		"frontend http\n    bind :80\n    mode http\n    http-request redirect scheme https code 301\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "{{") {
		t.Errorf("unrendered template syntax:\n%s", s)
	}
}

func TestApplyValidatesBeforeInstall(t *testing.T) {
	dir, st := newTestState(t)
	st.Upsert(&Service{Name: "a", Hosts: []string{"a.lan"}, Servers: []Server{{Addr: "127.0.0.1:1"}}}, false)
	dst := filepath.Join(dir, "haproxy.cfg")

	changed, err := apply(dir, st, false, false)
	if err != nil || !changed {
		t.Fatalf("first apply: changed=%v err=%v", changed, err)
	}
	if changed, _ := apply(dir, st, false, false); changed {
		t.Fatal("second apply should be a no-op")
	}

	good, _ := os.ReadFile(dst)
	st.Upsert(&Service{Name: "b", Hosts: []string{"b.lan"}, Servers: []Server{{Addr: "127.0.0.1:2"}}}, false)
	st.Settings.ValidateCmd = "false"
	if _, err := apply(dir, st, false, false); err == nil {
		t.Fatal("expected validation failure")
	}
	now, _ := os.ReadFile(dst)
	if string(now) != string(good) {
		t.Fatal("failed validation modified the installed config")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".haproxy.cfg.candidate.*")); len(left) > 0 {
		t.Fatalf("candidate files left behind: %v", left)
	}
}

func TestCLI(t *testing.T) {
	dir := t.TempDir()
	if err := run([]string{"--dir", dir, "init", "--output", "out.cfg", "--validate", "true", "--reload", "-"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--dir", dir, "add", "jf", "--host", "jf.lan", "--server", "main=127.0.0.1:8096", "--apply"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "out.cfg"))
	if err != nil || !strings.Contains(string(b), "server main 127.0.0.1:8096") {
		t.Fatalf("rendered: %s %v", b, err)
	}
	if err := run([]string{"--dir", dir, "rm", "jf"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--dir", dir, "rm", "jf"}); err == nil {
		t.Fatal("removing missing service should fail")
	}
}

func TestNormalizeAddr(t *testing.T) {
	good := map[string]string{
		"127.0.0.1:8096":          "127.0.0.1:8096",
		"[::1]:8080":              "[::1]:8080",
		"/run/app.sock":           "/run/app.sock",
		"unix@/run/app/http.sock": "/run/app/http.sock",
		"/run/a=b.sock":           "/run/a=b.sock",
	}
	for in, want := range good {
		if got, err := normalizeAddr(in); err != nil || got != want {
			t.Errorf("normalizeAddr(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"jellyfin:8096", "10.0.0.1:0", "relative.sock", "unix@relative.sock",
		"/run/dir/", "/run/../etc/x.sock", "/run/a b.sock", "/run/a#b.sock",
		"/" + strings.Repeat("a", 120),
	} {
		if _, err := normalizeAddr(in); err == nil {
			t.Errorf("normalizeAddr(%q): expected error", in)
		}
	}
}

func TestUnixSocketServer(t *testing.T) {
	dir := t.TempDir()
	if err := run([]string{"--dir", dir, "init", "--output", "out.cfg", "--validate", "true", "--reload", "-"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--dir", dir, "add", "git", "--host", "git.lan",
		"--server", "/run/forgejo/forgejo.sock", "--server", "b=unix@/run/b.sock", "--check", "/", "--apply"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "out.cfg"))
	for _, want := range []string{"server s1 /run/forgejo/forgejo.sock check", "server b /run/b.sock check"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %q in:\n%s", want, b)
		}
	}
}

func TestACLs(t *testing.T) {
	_, st := newTestState(t)
	if err := st.AddACLRule("lan", "src 192.168.0.0/16 10.0.0.0/8", false); err != nil {
		t.Fatal(err)
	}
	if err := st.AddACLRule("lan", "src 127.0.0.1", false); err != nil { // OR'ed second rule
		t.Fatal(err)
	}
	st.AddACLRule("blocked", "src 192.168.1.66", false)
	for _, bad := range [][2]string{{"x y", "src 1.2.3.4"}, {"ok", ""}, {"ok", "src 1.2.3.4\nfoo"}, {"TRUE", "src 1.2.3.4"}, {"lan", "src 127.0.0.1"}} {
		if err := st.AddACLRule(bad[0], bad[1], false); err == nil {
			t.Errorf("AddACLRule(%q, %q): expected error", bad[0], bad[1])
		}
	}

	// unknown acl reference is rejected
	if err := st.Upsert(&Service{Name: "nas", Hosts: []string{"nas.lan"}, Servers: []Server{{Addr: "10.0.0.5:5001"}}, ACLs: []string{"nope"}}, false); err == nil {
		t.Fatal("expected unknown acl error")
	}
	if err := st.Upsert(&Service{Name: "nas", Hosts: []string{"nas.lan"}, Servers: []Server{{Addr: "10.0.0.5:5001"}}, ACLs: []string{"lan"}}, false); err != nil {
		t.Fatal(err)
	}
	st.Upsert(&Service{Name: "open", Hosts: []string{"open.lan"}, Servers: []Server{{Addr: "10.0.0.6:80"}}}, false)
	if err := st.SetServiceACLs("nas", []string{"!blocked", "LOCALHOST"}, true); err != nil {
		t.Fatal(err)
	}
	if err := st.SetServiceACLs("nas", []string{"LOCALHOST"}, false); err != nil {
		t.Fatal(err)
	}
	if err := st.SetServiceACLs("nas", []string{"missing"}, true); err == nil {
		t.Fatal("attach of undefined acl should fail")
	}
	if err := st.RemoveACL("lan"); err == nil {
		t.Fatal("removing an acl in use should fail")
	}

	out, err := render(st, defaultTemplate, "test")
	if err != nil {
		t.Fatal(err)
	}
	s := normIndent(string(out))
	for _, want := range []string{
		"    acl blocked src 192.168.1.66\n",
		"    acl lan src 192.168.0.0/16 10.0.0.0/8\n    acl lan src 127.0.0.1\n",
		"use_backend be_nas if { var(txn.host) -m str nas.lan } lan !blocked\n",
		"use_backend be_open if { var(txn.host) -m str open.lan }\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Index(s, "\n    acl lan") > strings.Index(s, "\n    use_backend") {
		t.Error("acls must be declared before use_backend")
	}

	st.SetServiceACLs("nas", []string{"lan", "!blocked"}, false)
	if err := st.RemoveACL("lan"); err != nil {
		t.Fatal(err)
	}
}

func TestACLCLI(t *testing.T) {
	dir := t.TempDir()
	r := func(args ...string) error { return run(append([]string{"--dir", dir}, args...)) }
	if err := r("init", "--output", "out.cfg", "--validate", "true", "--reload", "-"); err != nil {
		t.Fatal(err)
	}
	// rule words like -m / -i must be kept verbatim; --apply is a flag anywhere
	if err := r("acl", "add", "curl", "req.hdr(user-agent)", "-m", "sub", "-i", "curl", "--apply"); err != nil {
		t.Fatal(err)
	}
	if err := r("add", "app", "--host", "app.lan", "--server", "127.0.0.1:1", "--acl", "!curl", "--apply"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "out.cfg"))
	for _, want := range []string{"acl curl req.hdr(user-agent) -m sub -i curl\n", "-m str app.lan } !curl\n"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %q in:\n%s", want, b)
		}
	}
	if err := r("acl", "add", "curl", "src", "1.2.3.4", "--replace"); err != nil {
		t.Fatal(err)
	}
	st, _ := loadState(dir)
	if _, a := st.FindACL("curl"); len(a.Rules) != 1 || a.Rules[0] != "src 1.2.3.4" {
		t.Fatalf("--replace: %+v", a)
	}
}

// normIndent rewrites every indented line to a 4-space indent so tests do
// not depend on the template's indentation style.
func normIndent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if t := strings.TrimLeft(l, " \t"); t != l {
			lines[i] = "    " + t
		}
	}
	return strings.Join(lines, "\n")
}

func TestSubdomains(t *testing.T) {
	_, st := newTestState(t)
	srv := []Server{{Addr: "127.0.0.1:8096"}}
	// --sub without any domain is rejected
	if err := st.Upsert(&Service{Name: "jf", Subdomains: []string{"jf"}, Servers: srv}, false); err == nil {
		t.Fatal("expected error: --sub without domain")
	}
	st.Settings.Domain = "Example.ORG."
	if err := st.Validate(); err != nil || st.Settings.Domain != "example.org" {
		t.Fatalf("domain not normalized: %q %v", st.Settings.Domain, err)
	}
	if err := st.Upsert(&Service{Name: "jf", Subdomains: []string{"JF", "media.home.lab", "@"}, Hosts: []string{"jf.other.net"}, Servers: srv}, false); err != nil {
		t.Fatal(err)
	}
	// per-service domain override
	if err := st.Upsert(&Service{Name: "nas", Subdomains: []string{"nas"}, Domain: "lan.internal", Servers: srv}, false); err != nil {
		t.Fatal(err)
	}
	_, jf := st.Find("jf")
	got, _ := st.AllHosts(jf)
	if want := "jf.other.net jf.example.org media.home.lab.example.org example.org"; strings.Join(got, " ") != want {
		t.Fatalf("AllHosts = %q, want %q", got, want)
	}
	// conflicts are detected on the expanded names
	if err := st.Upsert(&Service{Name: "dup", Hosts: []string{"media.home.lab.example.org"}, Servers: srv}, false); err == nil {
		t.Fatal("expected conflict between --host and expanded --sub")
	}
	for _, bad := range []string{"*.x", "a..b", "-a", "a b", ".a", strings.Repeat("a", 64)} {
		if err := st.Upsert(&Service{Name: "bad", Subdomains: []string{bad}, Servers: srv}, false); err == nil {
			t.Errorf("sub %q: expected error", bad)
		}
	}

	out, err := render(st, defaultTemplate, "test")
	if err != nil {
		t.Fatal(err)
	}
	s := normIndent(string(out))
	for _, want := range []string{
		"use_backend be_jf if { var(txn.host) -m str jf.other.net jf.example.org media.home.lab.example.org example.org }",
		"use_backend be_nas if { var(txn.host) -m str nas.lan.internal }",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	// stored relative: changing the domain moves the service
	st.Settings.Domain = "example.net"
	out, _ = render(st, defaultTemplate, "test")
	if !strings.Contains(string(out), "media.home.lab.example.net") {
		t.Error("domain change not reflected")
	}
	if strings.Contains(string(out), "nas.example.net") {
		t.Error("per-service domain override ignored")
	}
}

func TestSubdomainCLI(t *testing.T) {
	dir := t.TempDir()
	r := func(args ...string) error { return run(append([]string{"--dir", dir}, args...)) }
	if err := r("init", "--domain", "example.org", "--output", "out.cfg", "--validate", "true", "--reload", "-"); err != nil {
		t.Fatal(err)
	}
	if err := r("add", "jf", "--sub", "media.jellyfin", "--server", "127.0.0.1:8096", "--apply"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "out.cfg"))
	if !strings.Contains(string(b), "-m str media.jellyfin.example.org }") {
		t.Fatalf("not expanded:\n%s", b)
	}
	if err := r("domain", "--unset"); err == nil {
		t.Fatal("unsetting the domain while --sub services exist should fail")
	}
	if err := r("domain", "home.lan", "--apply"); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(dir, "out.cfg"))
	if !strings.Contains(string(b), "-m str media.jellyfin.home.lan }") {
		t.Fatalf("domain change not applied:\n%s", b)
	}
}

func TestServerOptions(t *testing.T) {
	for in, want := range map[string]Server{
		"127.0.0.1:8096":                         {Addr: "127.0.0.1:8096"},
		"127.0.0.1:8096 maxconn 100  alpn h2":    {Addr: "127.0.0.1:8096", Options: "maxconn 100  alpn h2"},
		"main=127.0.0.1:8096 weight 10":          {Name: "main", Addr: "127.0.0.1:8096", Options: "weight 10"},
		"/run/a=b.sock maxconn 5":                {Addr: "/run/a=b.sock", Options: "maxconn 5"},
		"s=unix@/run/x.sock proto h2":            {Name: "s", Addr: "unix@/run/x.sock", Options: "proto h2"},
		"  10.0.0.1:80   inter 2s fall 3 rise 2": {Addr: "10.0.0.1:80", Options: "inter 2s fall 3 rise 2"},
	} {
		got := parseServerSpec(in)
		want.Options = strings.Join(strings.Fields(want.Options), " ")
		if got.Options != "" {
			got.Options = strings.Join(strings.Fields(got.Options), " ")
		}
		if got != want {
			t.Errorf("parseServerSpec(%q) = %+v, want %+v", in, got, want)
		}
	}

	dir := t.TempDir()
	r := func(args ...string) error { return run(append([]string{"--dir", dir}, args...)) }
	r("init", "--output", "out.cfg", "--validate", "true", "--reload", "-")
	if err := r("add", "web", "--host", "web.lan", "--check", "/hz", "--ssl",
		"--server", "127.0.0.1:8081   maxconn 100 alpn h2", "--server", "b=127.0.0.1:8082 weight 50", "--apply"); err != nil {
		t.Fatal(err)
	}
	out := func() string { b, _ := os.ReadFile(filepath.Join(dir, "out.cfg")); return string(b) }
	for _, want := range []string{
		"server s1 127.0.0.1:8081 check ssl verify none maxconn 100 alpn h2\n",
		"server b 127.0.0.1:8082 check ssl verify none weight 50\n",
	} {
		if !strings.Contains(out(), want) {
			t.Errorf("missing %q in:\n%s", want, out())
		}
	}
	if err := r("server-opts", "web", "b", "maxconn", "20", "--apply"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out(), "server b 127.0.0.1:8082 check ssl verify none maxconn 20\n") {
		t.Errorf("server-opts not applied:\n%s", out())
	}
	if err := r("server-opts", "web", "b", "--apply"); err != nil { // clear
		t.Fatal(err)
	}
	if !strings.Contains(out(), "server b 127.0.0.1:8082 check ssl verify none\n") {
		t.Errorf("server-opts clear failed:\n%s", out())
	}
	if err := r("server-opts", "web", "nope", "maxconn", "1"); err == nil {
		t.Error("unknown server should fail")
	}
	if err := r("server-opts", "web", "s1", "maxconn 1 # x"); err == nil {
		t.Error("'#' in options should be rejected")
	}
}
