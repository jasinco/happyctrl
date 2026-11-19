package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// A service's hostnames come from two places:
//
//	Hosts       full names, used as is            --host git.example.org
//	Subdomains  relative names, joined to a       --sub git        -> git.<domain>
//	            domain suffix at render time      --sub a.b.c      -> a.b.c.<domain>
//	                                              --sub @          -> <domain> itself
//
// The suffix is the service's own Domain if set, else settings.domain. It is
// resolved when rendering, so changing settings.domain moves every service
// that uses relative names.

// One or more DNS labels separated by dots (no wildcard, no trailing dot).
var labelsRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// Apex is the --sub value meaning "the domain itself".
const Apex = "@"

// normalizeLabels lowercases and validates a dotted name such as a domain or
// a multi-level subdomain.
func normalizeLabels(kind, v string) (string, error) {
	v = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(v)), ".")
	if strings.HasPrefix(v, "*") {
		return "", fmt.Errorf("wildcard %s %q is not supported", kind, v)
	}
	if !labelsRe.MatchString(v) {
		return "", fmt.Errorf("invalid %s %q", kind, v)
	}
	for l := range strings.SplitSeq(v, ".") {
		if len(l) > 63 {
			return "", fmt.Errorf("%s %q: label %q is longer than 63 characters", kind, v, l)
		}
	}
	return v, nil
}

func normalizeSub(v string) (string, error) {
	if strings.TrimSpace(v) == Apex {
		return Apex, nil
	}
	return normalizeLabels("subdomain", v)
}

// domainFor returns the suffix used for s's subdomains ("" if none).
func (st *State) domainFor(s *Service) string {
	if s.Domain != "" {
		return s.Domain
	}
	return st.Settings.Domain
}

// AllHosts returns s's full hostnames: Hosts followed by the expanded
// Subdomains, without duplicates.
func (st *State) AllHosts(s *Service) ([]string, error) {
	out := make([]string, 0, len(s.Hosts)+len(s.Subdomains))
	seen := map[string]bool{}
	add := func(h string) error {
		if len(h) > 253 {
			return fmt.Errorf("service %q: hostname %q is longer than 253 characters", s.Name, h)
		}
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
		return nil
	}
	for _, h := range s.Hosts {
		if err := add(h); err != nil {
			return nil, err
		}
	}
	if len(s.Subdomains) > 0 {
		d := st.domainFor(s)
		if d == "" {
			return nil, fmt.Errorf("service %q uses --sub but no domain is set (set settings.domain with `happyctrl domain example.org`, or pass --domain)", s.Name)
		}
		for _, sub := range s.Subdomains {
			h := d
			if sub != Apex {
				h = sub + "." + d
			}
			if err := add(h); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// cmdDomain shows or changes settings.domain.
func cmdDomain(dir string, args []string) error {
	st, err := loadState(dir)
	if err != nil {
		return err
	}
	switch {
	case len(args) == 0:
		if st.Settings.Domain == "" {
			fmt.Println("(no domain set)")
		} else {
			fmt.Println(st.Settings.Domain)
		}
		return nil
	case len(args) > 2 || (len(args) == 2 && args[1] != "--apply"):
		return fmt.Errorf("usage: happyctrl domain [DOMAIN|--unset] [--apply]")
	}
	if args[0] == "--unset" {
		st.Settings.Domain = ""
	} else {
		d, err := normalizeLabels("domain", args[0])
		if err != nil {
			return err
		}
		st.Settings.Domain = d
	}
	if err := st.Validate(); err != nil {
		return err
	}
	if err := st.Save(dir); err != nil {
		return err
	}
	if st.Settings.Domain == "" {
		fmt.Fprintln(os.Stderr, "domain unset")
	} else {
		fmt.Fprintln(os.Stderr, "domain set to", st.Settings.Domain)
	}
	if len(args) == 2 {
		return applyAndReport(dir, st, false, false)
	}
	return nil
}
