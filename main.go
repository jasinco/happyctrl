// happyctrl renders an HAProxy config from a template and a small list of
// proxied services (typically Docker containers in a homelab), then validates,
// installs and reloads it.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
)

const usage = `happyctrl — manage an HAProxy reverse proxy for homelab services

Usage:
  happyctrl [--dir DIR] <command> [flags]

Commands:
  init      create happyctrl.json and haproxy.cfg.tmpl in DIR
  add       add (or --replace) a proxied service
  rm        remove a service
  ls        list services
  server-opts  set the options appended to a server line (maxconn, alpn, ...)
  domain    show or set the domain suffix used by --sub
  acl       manage named acls and attach them to services (happyctrl acl help)
  render    print the rendered haproxy.cfg to stdout
  apply     render, validate, install and reload haproxy
  version   print the version

DIR defaults to $HAPPYCTRL_DIR, else the current directory.
Run 'happyctrl <command> -h' for command flags.

Examples:
  happyctrl init
  happyctrl add jellyfin --host jellyfin.home.lan --server 127.0.0.1:8096
  happyctrl add web --sub www --server '127.0.0.1:8081 maxconn 100 alpn h2' --server '127.0.0.1:8082 weight 50'
  happyctrl server-opts web s1 maxconn 200 alpn h2,http/1.1
  happyctrl domain example.org
  happyctrl add media --sub jellyfin --sub media.home --server 127.0.0.1:8096
            # -> jellyfin.example.org, media.home.example.org
  happyctrl add nas --host nas.home.lan --server 192.168.1.20:5001 --ssl
  happyctrl add git --host git.home.lan --server /run/forgejo/forgejo.sock
  happyctrl add web --host www.home.lan --server 127.0.0.1:8081 --server 127.0.0.1:8082 --check /health
  happyctrl acl add lan src 192.168.0.0/16 10.0.0.0/8 127.0.0.1
  happyctrl add nas --host nas.home.lan --server 192.168.1.20:5001 --ssl --acl lan --replace
  sudo happyctrl apply

HAProxy runs on the host: --server is the IP:port or unix socket path a
service listens on (the haproxy user needs access to the socket), e.g.
the published port of a container (-p 8096:8096 -> 127.0.0.1:8096).
`

// version is set at build time: -ldflags "-X main.version=...".
var version = "dev"

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "happyctrl:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	g := flag.NewFlagSet("happyctrl", flag.ContinueOnError)
	g.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	dir := g.String("dir", envOr("HAPPYCTRL_DIR", "."), "config directory")
	if err := g.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if g.NArg() == 0 {
		g.Usage()
		return errors.New("no command given")
	}
	cmd, rest := g.Arg(0), g.Args()[1:]
	switch cmd {
	case "init":
		return cmdInit(*dir, rest)
	case "add":
		return cmdAdd(*dir, rest)
	case "rm", "remove":
		return cmdRm(*dir, rest)
	case "ls", "list":
		return cmdLs(*dir, rest)
	case "server-opts":
		return cmdServerOpts(*dir, rest)
	case "domain":
		return cmdDomain(*dir, rest)
	case "acl":
		return cmdACL(*dir, rest)
	case "render":
		return cmdRender(*dir, rest)
	case "apply":
		return cmdApply(*dir, rest)
	case "version", "--version":
		fmt.Println("happyctrl", version)
		return nil
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q (see happyctrl help)", cmd)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// parseWithName parses flags allowing the positional NAME before or after flags.
func parseWithName(fs *flag.FlagSet, args []string) (string, error) {
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if name == "" && fs.NArg() > 0 {
		name = fs.Arg(0)
	} else if fs.NArg() > 0 {
		return "", fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if name == "" {
		return "", errors.New("service NAME is required")
	}
	return name, nil
}

func cmdInit(dir string, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	force := fs.Bool("force", false, "overwrite existing files")
	output := fs.String("output", "", "path apply writes haproxy.cfg to (default /etc/haproxy/haproxy.cfg)")
	reload := fs.String("reload", "", `reload command (default "systemctl reload haproxy"; "-" for none)`)
	validate := fs.String("validate", "", `validate command ({file} = candidate config); default "haproxy -c -f {file}"`)
	domain := fs.String("domain", "", "domain suffix for services added with --sub, e.g. example.org")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	st := defaultState()
	if *domain != "" {
		d, err := normalizeLabels("domain", *domain)
		if err != nil {
			return err
		}
		st.Settings.Domain = d
	}
	if *output != "" {
		st.Settings.OutputPath = *output
	}
	if *reload == "-" {
		st.Settings.ReloadCmd = ""
	} else if *reload != "" {
		st.Settings.ReloadCmd = *reload
	}
	if *validate != "" {
		st.Settings.ValidateCmd = *validate
	}
	files := map[string]func() error{
		stateFile: func() error { return st.Save(dir) },
		templateFile: func() error {
			return writeFileAtomic(filepath.Join(dir, templateFile), []byte(defaultTemplate), 0o644)
		},
	}
	for _, name := range []string{stateFile, templateFile} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil && !*force {
			fmt.Fprintf(os.Stderr, "kept existing %s (use --force to overwrite)\n", p)
			continue
		}
		if err := files[name](); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s\n", p)
	}
	return nil
}

func cmdAdd(dir string, args []string) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: happyctrl add NAME (--host FQDN | --sub SUBDOMAIN)... --server ADDR [--server ADDR...] [flags]")
		fs.PrintDefaults()
	}
	var hosts, servers, extra multiFlag
	var subs multiFlag
	fs.Var(&hosts, "host", "full hostname routed to this service (repeatable)")
	fs.Var(&subs, "sub", "subdomain of the domain, may be several levels (git, media.jellyfin) or @ for the domain itself (repeatable)")
	domain := fs.String("domain", "", "domain suffix for --sub (default settings.domain)")
	fs.Var(&servers, "server", "backend \"[name=]ADDR [OPTIONS...]\": ADDR is IP:port or a unix socket path, OPTIONS are appended to the haproxy server line, e.g. '127.0.0.1:8096 maxconn 100 alpn h2' (repeatable)")
	fs.Var(&extra, "extra", "raw haproxy line appended to the backend (repeatable)")
	var acls multiFlag
	fs.Var(&acls, "acl", "named acl that must also match, !name to negate (repeatable)")
	check := fs.String("check", "", "HTTP health check path, e.g. /health")
	ssl := fs.Bool("ssl", false, "backend speaks HTTPS (certificate not verified)")
	balance := fs.String("balance", "", "balance algorithm (roundrobin, leastconn, source, ...)")
	replace := fs.Bool("replace", false, "replace an existing service with the same name")
	doApply := fs.Bool("apply", false, "run apply afterwards")
	name, err := parseWithName(fs, args)
	if err != nil {
		return err
	}
	st, err := loadState(dir)
	if err != nil {
		return err
	}
	svc := &Service{Name: name, Hosts: hosts, Subdomains: subs, Domain: *domain, HealthCheck: *check, ServerSSL: *ssl, Balance: *balance, Extra: extra, ACLs: acls}
	for _, s := range servers {
		svc.Servers = append(svc.Servers, parseServerSpec(s))
	}
	if err := st.Upsert(svc, *replace); err != nil {
		return err
	}
	if err := st.Save(dir); err != nil {
		return err
	}
	all, _ := st.AllHosts(svc)
	fmt.Fprintf(os.Stderr, "added %s: %s -> %s\n", svc.Name, strings.Join(all, ","), serverList(svc))
	if *doApply {
		return applyAndReport(dir, st, false, false)
	}
	return nil
}

func cmdRm(dir string, args []string) error {
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	doApply := fs.Bool("apply", false, "run apply afterwards")
	name, err := parseWithName(fs, args)
	if err != nil {
		return err
	}
	st, err := loadState(dir)
	if err != nil {
		return err
	}
	if err := st.Remove(name); err != nil {
		return err
	}
	if err := st.Save(dir); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "removed %s\n", name)
	if *doApply {
		return applyAndReport(dir, st, false, false)
	}
	return nil
}

func cmdLs(dir string, args []string) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := loadState(dir)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tHOSTS\tSERVERS\tCHECK\tACLS")
	for _, s := range st.Services {
		chk := s.HealthCheck
		if chk == "" {
			chk = "-"
		}
		all, err := st.AllHosts(s)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.Name, strings.Join(all, ","), serverList(s), chk, aclListString(s.ACLs))
	}
	return w.Flush()
}

func serverList(s *Service) string {
	parts := make([]string, len(s.Servers))
	for i, sv := range s.Servers {
		parts[i] = sv.Addr
		if sv.Options != "" {
			parts[i] += " [" + sv.Options + "]"
		}
	}
	out := strings.Join(parts, ",")
	if s.ServerSSL {
		out += " (https)"
	}
	return out
}

func cmdRender(dir string, args []string) error {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := loadState(dir)
	if err != nil {
		return err
	}
	src, path, err := loadTemplate(dir)
	if err != nil {
		return err
	}
	out, err := render(st, src, path)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(out)
	return err
}

func cmdApply(dir string, args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "only report whether the config would change")
	force := fs.Bool("force", false, "validate, write and reload even if unchanged")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := loadState(dir)
	if err != nil {
		return err
	}
	return applyAndReport(dir, st, *dry, *force)
}

func applyAndReport(dir string, st *State, dry, force bool) error {
	changed, err := apply(dir, st, dry, force)
	if err != nil {
		return err
	}
	switch {
	case !changed:
		fmt.Fprintln(os.Stderr, "haproxy config unchanged")
	case dry:
		fmt.Fprintln(os.Stderr, "haproxy config would change")
	default:
		fmt.Fprintln(os.Stderr, "haproxy config applied")
	}
	return nil
}

// cmdServerOpts replaces the options of one server: server-opts SERVICE SERVER [OPTIONS...]
// With no OPTIONS the server's options are cleared.
func cmdServerOpts(dir string, args []string) error {
	var doApply bool
	var pos []string
	for _, a := range args {
		if a == "--apply" || a == "-apply" {
			doApply = true
			continue
		}
		pos = append(pos, a)
	}
	if len(pos) < 2 {
		return errors.New("usage: happyctrl server-opts SERVICE SERVER [OPTIONS...] [--apply]   (no OPTIONS clears them)")
	}
	st, err := loadState(dir)
	if err != nil {
		return err
	}
	_, svc := st.Find(pos[0])
	if svc == nil {
		return fmt.Errorf("no service named %q", pos[0])
	}
	var sv *Server
	var names []string
	for i := range svc.Servers {
		names = append(names, svc.Servers[i].Name)
		if svc.Servers[i].Name == pos[1] {
			sv = &svc.Servers[i]
		}
	}
	if sv == nil {
		return fmt.Errorf("service %q has no server %q (servers: %s)", svc.Name, pos[1], strings.Join(names, ", "))
	}
	opts, err := normalizeServerOptions(strings.Join(pos[2:], " "))
	if err != nil {
		return err
	}
	sv.Options = opts
	if err := st.Save(dir); err != nil {
		return err
	}
	if opts == "" {
		fmt.Fprintf(os.Stderr, "%s/%s: options cleared\n", svc.Name, sv.Name)
	} else {
		fmt.Fprintf(os.Stderr, "%s/%s: %s %s\n", svc.Name, sv.Name, sv.Addr, opts)
	}
	if doApply {
		return applyAndReport(dir, st, false, false)
	}
	return nil
}
