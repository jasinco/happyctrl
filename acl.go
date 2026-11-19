package main

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
)

// ACL is a named HAProxy ACL rendered into the frontend as
//
//	acl <Name> <rule>
//
// one line per rule. HAProxy ORs repeated declarations of the same name, so
// an ACL with several rules matches when any of them matches.
type ACL struct {
	Name  string   `json:"name"`
	Rules []string `json:"rules"`
}

// HAProxy's predefined ACLs: they can be referenced by services but not
// redefined.
var predefinedACLs = map[string]bool{
	"FALSE": true, "TRUE": true, "LOCALHOST": true, "HTTP": true,
	"HTTP_1.0": true, "HTTP_1.1": true, "HTTP_2.0": true, "HTTP_3.0": true,
	"HTTP_CONTENT": true, "HTTP_URL_ABS": true, "HTTP_URL_SLASH": true, "HTTP_URL_STAR": true,
	"METH_CONNECT": true, "METH_DELETE": true, "METH_GET": true, "METH_HEAD": true,
	"METH_OPTIONS": true, "METH_POST": true, "METH_PUT": true, "METH_TRACE": true,
	"RDP_COOKIE": true, "REQ_CONTENT": true, "WAIT_END": true,
}

// HAProxy ACL names: letters, digits, '-', '_', '.' and ':'.
var aclNameRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)

func (a *ACL) Normalize() error {
	if !aclNameRe.MatchString(a.Name) {
		return fmt.Errorf("invalid acl name %q (use letters, digits, _ . : -)", a.Name)
	}
	if predefinedACLs[a.Name] {
		return fmt.Errorf("acl %q is predefined by HAProxy and cannot be redefined", a.Name)
	}
	if len(a.Rules) == 0 {
		return fmt.Errorf("acl %q: at least one rule is required", a.Name)
	}
	for i, r := range a.Rules {
		r = strings.TrimSpace(r)
		if r == "" || unsafeRe.MatchString(r) {
			return fmt.Errorf("acl %q: rule %q must be a single non-empty line without '#'", a.Name, a.Rules[i])
		}
		a.Rules[i] = r
	}
	return nil
}

// splitACLRef splits "!name" into ("name", true).
func splitACLRef(ref string) (name string, negated bool) {
	if strings.HasPrefix(ref, "!") {
		return ref[1:], true
	}
	return ref, false
}

func (st *State) FindACL(name string) (int, *ACL) {
	for i, a := range st.ACLs {
		if a.Name == name {
			return i, a
		}
	}
	return -1, nil
}

// aclUsers returns the services that reference the named ACL.
func (st *State) aclUsers(name string) []string {
	var users []string
	for _, s := range st.Services {
		for _, ref := range s.ACLs {
			if n, _ := splitACLRef(ref); n == name {
				users = append(users, s.Name)
				break
			}
		}
	}
	return users
}

// validateACLs checks the ACL list and every service's ACL references.
func (st *State) validateACLs() error {
	defined := map[string]bool{}
	for _, a := range st.ACLs {
		if err := a.Normalize(); err != nil {
			return err
		}
		if defined[a.Name] {
			return fmt.Errorf("duplicate acl %q", a.Name)
		}
		defined[a.Name] = true
	}
	for _, s := range st.Services {
		for _, ref := range s.ACLs {
			n, _ := splitACLRef(ref)
			if !defined[n] && !predefinedACLs[n] {
				return fmt.Errorf("service %q uses unknown acl %q (create it with `happyctrl acl add %s RULE`)", s.Name, n, n)
			}
		}
	}
	return nil
}

func normalizeACLRefs(refs []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		n, _ := splitACLRef(ref)
		if !aclNameRe.MatchString(n) {
			return nil, fmt.Errorf("invalid acl reference %q", ref)
		}
		if !seen[ref] {
			seen[ref] = true
			out = append(out, ref)
		}
	}
	return out, nil
}

const aclUsage = `Usage:
  happyctrl acl add NAME RULE...       add a rule to acl NAME (creates it; several rules are OR'ed)
      --replace                        replace all existing rules of NAME instead of appending
  happyctrl acl rm NAME                delete acl NAME (must not be in use)
  happyctrl acl ls                     list acls and the services using them
  happyctrl acl attach SERVICE ACL...  require ACL(s) for SERVICE; prefix with ! to negate
  happyctrl acl detach SERVICE ACL...  stop requiring ACL(s) for SERVICE

The rule is everything after NAME, written as in haproxy, e.g.
  happyctrl acl add lan src 192.168.0.0/16 10.0.0.0/8 127.0.0.1
  happyctrl acl add office src 203.0.113.7
  happyctrl acl attach nas lan
A service with acls is only routed when its host matches AND all of its acls
match; otherwise the request falls through to the default backend (404).
All commands accept --apply.
`

func cmdACL(dir string, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(os.Stderr, aclUsage)
		if len(args) == 0 {
			return fmt.Errorf("acl: missing subcommand")
		}
		return nil
	}
	sub, args := args[0], args[1:]
	// Only --replace and --apply are flags; every other word is positional and
	// kept verbatim, because haproxy rules contain options like "-i" or "-m".
	var replace, doApply bool
	var pos []string
	for _, a := range args {
		switch a {
		case "--replace", "-replace":
			replace = true
		case "--apply", "-apply":
			doApply = true
		default:
			pos = append(pos, a)
		}
	}

	st, err := loadState(dir)
	if err != nil {
		return err
	}
	switch sub {
	case "add":
		if len(pos) < 2 {
			return fmt.Errorf("usage: happyctrl acl add NAME RULE...")
		}
		name, rule := pos[0], strings.Join(pos[1:], " ")
		if err := st.AddACLRule(name, rule, replace); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "acl %s %s\n", name, rule)
	case "rm", "remove":
		if len(pos) != 1 {
			return fmt.Errorf("usage: happyctrl acl rm NAME")
		}
		if err := st.RemoveACL(pos[0]); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "removed acl %s\n", pos[0])
	case "ls", "list":
		return printACLs(st)
	case "attach", "detach":
		if len(pos) < 2 {
			return fmt.Errorf("usage: happyctrl acl %s SERVICE ACL...", sub)
		}
		if err := st.SetServiceACLs(pos[0], pos[1:], sub == "attach"); err != nil {
			return err
		}
		_, svc := st.Find(pos[0])
		fmt.Fprintf(os.Stderr, "%s requires: %s\n", svc.Name, aclListString(svc.ACLs))
	default:
		return fmt.Errorf("unknown acl subcommand %q\n%s", sub, aclUsage)
	}
	if err := st.Save(dir); err != nil {
		return err
	}
	if doApply {
		return applyAndReport(dir, st, false, false)
	}
	return nil
}

func (st *State) AddACLRule(name, rule string, replace bool) error {
	_, a := st.FindACL(name)
	next := &ACL{Name: name}
	if a != nil && !replace {
		next.Rules = append(next.Rules, a.Rules...)
	}
	rule = strings.TrimSpace(rule)
	for _, r := range next.Rules {
		if r == rule {
			return fmt.Errorf("acl %q already has rule %q", name, rule)
		}
	}
	next.Rules = append(next.Rules, rule)
	if err := next.Normalize(); err != nil {
		return err
	}
	if a != nil {
		*a = *next
	} else {
		st.ACLs = append(st.ACLs, next)
	}
	st.sort()
	return nil
}

func (st *State) RemoveACL(name string) error {
	i, _ := st.FindACL(name)
	if i < 0 {
		return fmt.Errorf("no acl named %q", name)
	}
	if users := st.aclUsers(name); len(users) > 0 {
		return fmt.Errorf("acl %q is used by %s; detach it first", name, strings.Join(users, ", "))
	}
	st.ACLs = append(st.ACLs[:i], st.ACLs[i+1:]...)
	return nil
}

// SetServiceACLs attaches (or detaches) ACL references on a service.
func (st *State) SetServiceACLs(service string, refs []string, attach bool) error {
	_, svc := st.Find(service)
	if svc == nil {
		return fmt.Errorf("no service named %q", service)
	}
	refs, err := normalizeACLRefs(refs)
	if err != nil {
		return err
	}
	cur := append([]string(nil), svc.ACLs...)
	if attach {
		cur = append(cur, refs...)
	} else {
		drop := map[string]bool{}
		for _, r := range refs {
			drop[r] = true
		}
		var kept []string
		for _, r := range cur {
			if !drop[r] {
				kept = append(kept, r)
			}
		}
		if len(kept) == len(cur) {
			return fmt.Errorf("service %q does not use %s", service, strings.Join(refs, ", "))
		}
		cur = kept
	}
	cur, _ = normalizeACLRefs(cur)
	old := svc.ACLs
	svc.ACLs = cur
	if err := st.validateACLs(); err != nil {
		svc.ACLs = old
		return err
	}
	return nil
}

func aclListString(refs []string) string {
	if len(refs) == 0 {
		return "-"
	}
	return strings.Join(refs, " ")
}

func printACLs(st *State) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tRULE\tUSED BY")
	for _, a := range st.ACLs {
		users := strings.Join(st.aclUsers(a.Name), ",")
		if users == "" {
			users = "-"
		}
		for i, r := range a.Rules {
			n, u := a.Name, users
			if i > 0 {
				n, u = "", "" // continuation line: OR'ed rule of the same acl
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", n, r, u)
		}
	}
	return w.Flush()
}

func (st *State) sortACLs() {
	sort.Slice(st.ACLs, func(i, j int) bool { return st.ACLs[i].Name < st.ACLs[j].Name })
}
