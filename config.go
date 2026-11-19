package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	stateFile    = "happyctrl.json"
	templateFile = "haproxy.cfg.tmpl"
)

// Settings control how happyctrl installs the config. HAProxy's own settings
// (ports, certs, timeouts, ...) live in the template, not here.
type Settings struct {
	// Where `apply` writes the rendered config.
	OutputPath string `json:"output_path"`
	// Shell command run against the candidate config before installing it.
	// {file} is replaced with the (quoted) path of the candidate file.
	ValidateCmd string `json:"validate_cmd"`
	// Shell command run after the config is installed, e.g.
	// "docker kill -s HUP haproxy" or "systemctl reload haproxy".
	ReloadCmd string `json:"reload_cmd"`
	// Suffix for services' relative hostnames (--sub), e.g. "example.org".
	Domain string `json:"domain,omitempty"`
}

type Server struct {
	Name string `json:"name"`
	Addr string `json:"addr"` // IP:port (127.0.0.1:8096, [::1]:8096) or unix socket path (/run/app.sock)
	// Raw haproxy server options appended to the server line,
	// e.g. "maxconn 100 alpn h2 weight 10".
	Options string `json:"options,omitempty"`
}

// parseServerSpec parses "[name=]ADDR [OPTIONS...]" as given to --server.
func parseServerSpec(spec string) Server {
	fields := strings.Fields(spec)
	if len(fields) == 0 {
		return Server{}
	}
	sv := Server{Addr: fields[0], Options: strings.Join(fields[1:], " ")}
	// name=addr, unless the address itself is a socket path (which may contain '=').
	if n, a, ok := strings.Cut(fields[0], "="); ok && !strings.HasPrefix(fields[0], "/") && !strings.HasPrefix(fields[0], "unix@") {
		sv.Name, sv.Addr = n, a
	}
	return sv
}

// normalizeServerOptions collapses whitespace and rejects anything that could
// break out of the server line. haproxy -c checks the keywords themselves.
func normalizeServerOptions(o string) (string, error) {
	if unsafeRe.MatchString(o) {
		return "", fmt.Errorf("server options %q must be a single line without '#'", o)
	}
	return strings.Join(strings.Fields(o), " "), nil
}

type Service struct {
	Name        string   `json:"name"`
	Hosts       []string `json:"hosts,omitempty"`      // full hostnames
	Subdomains  []string `json:"subdomains,omitempty"` // relative to Domain / settings.domain; "@" = the domain itself
	Domain      string   `json:"domain,omitempty"`     // overrides settings.domain for Subdomains
	Servers     []Server `json:"servers"`
	HealthCheck string   `json:"health_check,omitempty"` // URI; enables "check" on servers
	ServerSSL   bool     `json:"server_ssl,omitempty"`   // backend speaks HTTPS (self-signed ok)
	Balance     string   `json:"balance,omitempty"`
	Extra       []string `json:"extra,omitempty"` // raw lines appended to the backend
	// Named ACLs (or "!name") that must all match, in addition to the host,
	// for a request to be routed here.
	ACLs []string `json:"acls,omitempty"`
}

type State struct {
	Settings Settings   `json:"settings"`
	ACLs     []*ACL     `json:"acls"`
	Services []*Service `json:"services"`
}

func defaultState() *State {
	return &State{
		Settings: Settings{
			OutputPath:  "/etc/haproxy/haproxy.cfg",
			ValidateCmd: "haproxy -c -f {file}",
			ReloadCmd:   "systemctl reload haproxy",
		},
		ACLs:     []*ACL{},
		Services: []*Service{},
	}
}

func loadState(dir string) (*State, error) {
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s not found in %s (run `happyctrl init` first)", stateFile, dir)
	}
	if err != nil {
		return nil, err
	}
	st := defaultState()
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", stateFile, err)
	}
	if err := st.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", stateFile, err)
	}
	return st, nil
}

func (st *State) Save(dir string) error {
	st.sort()
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, stateFile), append(b, '\n'), 0o644)
}

func (st *State) sort() {
	sort.Slice(st.Services, func(i, j int) bool { return st.Services[i].Name < st.Services[j].Name })
	st.sortACLs()
}

func (st *State) Find(name string) (int, *Service) {
	for i, s := range st.Services {
		if s.Name == name {
			return i, s
		}
	}
	return -1, nil
}

// Upsert adds svc, or replaces an existing service of the same name when
// replace is true.
func (st *State) Upsert(svc *Service, replace bool) error {
	if err := svc.Normalize(); err != nil {
		return err
	}
	i, _ := st.Find(svc.Name)
	if i >= 0 && !replace {
		return fmt.Errorf("service %q already exists (use --replace)", svc.Name)
	}
	next := &State{Settings: st.Settings, ACLs: st.ACLs}
	for _, s := range st.Services {
		if s.Name != svc.Name {
			next.Services = append(next.Services, s)
		}
	}
	next.Services = append(next.Services, svc)
	if err := next.Validate(); err != nil {
		return err
	}
	st.Services = next.Services
	st.sort()
	return nil
}

func (st *State) Remove(name string) error {
	i, _ := st.Find(name)
	if i < 0 {
		return fmt.Errorf("no service named %q", name)
	}
	st.Services = append(st.Services[:i], st.Services[i+1:]...)
	return nil
}

var (
	nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	// Anything that could break out of a single config line.
	unsafeRe = regexp.MustCompile(`[\r\n#]`)
)

// Normalize lowercases hosts, names unnamed servers and validates the service.
func (s *Service) Normalize() error {
	if !nameRe.MatchString(s.Name) {
		return fmt.Errorf("invalid service name %q (use letters, digits, _ . -)", s.Name)
	}
	if s.Name == "notfound" {
		return errors.New(`service name "notfound" is reserved`)
	}
	if len(s.Hosts)+len(s.Subdomains) == 0 {
		return fmt.Errorf("service %q: at least one --host or --sub is required", s.Name)
	}
	if len(s.Servers) == 0 {
		return fmt.Errorf("service %q: at least one --server is required", s.Name)
	}
	for i, h := range s.Hosts {
		n, err := normalizeLabels("host", h)
		if err != nil {
			return fmt.Errorf("service %q: %w", s.Name, err)
		}
		s.Hosts[i] = n
	}
	for i, v := range s.Subdomains {
		n, err := normalizeSub(v)
		if err != nil {
			return fmt.Errorf("service %q: %w", s.Name, err)
		}
		s.Subdomains[i] = n
	}
	if s.Domain != "" {
		d, err := normalizeLabels("domain", s.Domain)
		if err != nil {
			return fmt.Errorf("service %q: %w", s.Name, err)
		}
		s.Domain = d
	}
	seen := map[string]bool{}
	for i := range s.Servers {
		sv := &s.Servers[i]
		if sv.Name == "" {
			sv.Name = fmt.Sprintf("s%d", i+1)
		}
		if !nameRe.MatchString(sv.Name) {
			return fmt.Errorf("service %q: invalid server name %q", s.Name, sv.Name)
		}
		if seen[sv.Name] {
			return fmt.Errorf("service %q: duplicate server name %q", s.Name, sv.Name)
		}
		seen[sv.Name] = true
		addr, err := normalizeAddr(sv.Addr)
		if err != nil {
			return fmt.Errorf("service %q: %w", s.Name, err)
		}
		sv.Addr = addr
		opts, err := normalizeServerOptions(sv.Options)
		if err != nil {
			return fmt.Errorf("service %q, server %q: %w", s.Name, sv.Name, err)
		}
		sv.Options = opts
	}
	if s.HealthCheck != "" && (!strings.HasPrefix(s.HealthCheck, "/") || strings.ContainsAny(s.HealthCheck, " \t\r\n")) {
		return fmt.Errorf("service %q: health check must be a path like /health", s.Name)
	}
	if s.Balance != "" && !nameRe.MatchString(s.Balance) {
		return fmt.Errorf("service %q: invalid balance %q", s.Name, s.Balance)
	}
	refs, err := normalizeACLRefs(s.ACLs)
	if err != nil {
		return fmt.Errorf("service %q: %w", s.Name, err)
	}
	s.ACLs = refs
	for _, line := range s.Extra {
		if unsafeRe.MatchString(line) {
			return fmt.Errorf("service %q: extra line %q must be a single line without '#'", s.Name, line)
		}
	}
	return nil
}

// normalizeAddr accepts either IP:port or an absolute unix socket path
// (optionally written as unix@/path) and returns the form used in the config.
func normalizeAddr(a string) (string, error) {
	if p, ok := strings.CutPrefix(a, "unix@"); ok || strings.HasPrefix(a, "/") {
		if ok {
			a = p
		}
		if !strings.HasPrefix(a, "/") || strings.HasSuffix(a, "/") {
			return "", fmt.Errorf("unix socket %q must be an absolute file path", a)
		}
		for _, r := range a {
			if r <= ' ' || r == '#' || r == 0x7f {
				return "", fmt.Errorf("unix socket path %q contains spaces, control characters or '#'", a)
			}
		}
		if c := filepath.Clean(a); c != a {
			return "", fmt.Errorf("unix socket path %q is not clean (did you mean %q?)", a, c)
		}
		if len(a) > 107 { // sun_path limit on Linux, incl. NUL
			return "", fmt.Errorf("unix socket path %q is too long (max 107 bytes)", a)
		}
		return a, nil
	}
	host, port, err := net.SplitHostPort(a)
	if err != nil || net.ParseIP(host) == nil {
		return "", fmt.Errorf("server address %q must be IP:port (e.g. 127.0.0.1:8096) or a unix socket path (e.g. /run/app.sock)", a)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return "", fmt.Errorf("server address %q has an invalid port", a)
	}
	return a, nil
}

// Validate checks the whole state, including cross-service host conflicts.
func (st *State) Validate() error {
	if st.Settings.Domain != "" {
		d, err := normalizeLabels("settings.domain", st.Settings.Domain)
		if err != nil {
			return err
		}
		st.Settings.Domain = d
	}
	owner := map[string]string{}
	names := map[string]bool{}
	for _, s := range st.Services {
		if err := s.Normalize(); err != nil {
			return err
		}
		if names[s.Name] {
			return fmt.Errorf("duplicate service %q", s.Name)
		}
		names[s.Name] = true
		hosts, err := st.AllHosts(s)
		if err != nil {
			return err
		}
		for _, h := range hosts {
			if o, ok := owner[h]; ok && o != s.Name {
				return fmt.Errorf("host %q is used by both %q and %q", h, o, s.Name)
			}
			owner[h] = s.Name
		}
	}
	return st.validateACLs()
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
