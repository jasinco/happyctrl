# happyctrl

Small CLI that manages an HAProxy reverse proxy **running on the host** for a
homelab: you keep a list of services (hostname [+ named ACLs] → backend `IP:port` or unix socket path), it
renders `haproxy.cfg` from a Go `text/template`, validates it with
`haproxy -c`, installs it atomically and reloads HAProxy.

## Quick start

```sh
go build -o happyctrl .
sudo mkdir -p /etc/happyctrl && cd /etc/happyctrl
sudo happyctrl --dir /etc/happyctrl init              # writes happyctrl.json + haproxy.cfg.tmpl
export HAPPYCTRL_DIR=/etc/happyctrl

# servers are IP:port (e.g. a container's published port) or a unix socket path
sudo -E happyctrl add jellyfin --host jellyfin.home.lan --server 127.0.0.1:8096
sudo -E happyctrl add nas      --host nas.home.lan --server 192.168.1.20:5001 --ssl
sudo -E happyctrl add web      --host www.home.lan --host home.lan \
     --server 127.0.0.1:8081 --server 127.0.0.1:8082 --check /health
sudo -E happyctrl add git      --host git.home.lan --server /run/forgejo/forgejo.sock
sudo -E happyctrl ls
sudo -E happyctrl render        # preview
sudo -E happyctrl apply         # write /etc/haproxy/haproxy.cfg, validate, reload
```

`add`/`rm` take `--apply` to apply immediately.

## Build, package, ship

```sh
make                         # gofmt/vet + tests + ./happyctrl
make dist                    # dist/happyctrl-<ver>-linux-{amd64,arm64}.tar.gz + SHA256SUMS
make install                 # PREFIX=/usr/local DESTDIR=
make ship   HOST=me@server   # build for the server's arch, install to /usr/local/bin
make deploy HOST=me@server   # ship + templates/haproxy.cfg.tmpl -> /etc/happyctrl
                             #   (old template kept as .bak), then init + apply
```

`ship`/`deploy` use ssh + scp and `sudo` on the server (you may be asked for
the sudo password). Overridable: `REMOTE_BIN`, `REMOTE_DIR`, `SSH`, `SCP`,
`SUDO`, `VERSION` (default: `git describe`, else `0.1.0-dev`), `ARCHES`.
`deploy` is safe to repeat: `init` never overwrites `happyctrl.json`, and
`apply` validates with `haproxy -c` before installing the config.
`happyctrl version` shows what is installed.

## Commands

| command    | what it does |
|------------|--------------|
| `init`     | create `happyctrl.json` and `haproxy.cfg.tmpl` (`--output`, `--reload`, `--validate`) |
| `add NAME` | `--host FQDN` and/or `--sub SUBDOMAIN` (repeat), `--domain` (per-service suffix), `--server '[name=]ADDR [OPTIONS...]'` where ADDR is `IP:port` or `/path/to.sock` (repeat), `--check /path`, `--ssl`, `--balance`, `--extra 'raw line'`, `--acl NAME` / `--acl '!NAME'` (repeat), `--replace` |
| `acl ...`  | named acls: `acl add NAME RULE...` (`--replace`), `acl rm`, `acl ls`, `acl attach SERVICE ACL...`, `acl detach SERVICE ACL...` |
| `domain`   | show the domain suffix; `domain example.org` sets it, `domain --unset` clears it (`--apply`) |
| `server-opts SERVICE SERVER [OPTIONS...]` | replace the options of one server line; no OPTIONS clears them (`--apply`) |
| `rm NAME`  | remove a service |
| `ls`       | list services |
| `render`   | print the config |
| `apply`    | render → write candidate → `validate_cmd` → atomic rename → `reload_cmd`. No-op if unchanged (`--force`, `--dry-run`) |

## Health checks

Every server line gets `check`: a TCP connect check, or an HTTP check on the
path given with `--check /path`. Down servers are taken out of rotation, and
their state (UP/DOWN, last check result) is available wherever you read
HAProxy's stats, e.g. the stats socket or a stats/metrics frontend you add to
the template.

## Server options

Anything after the address in `--server` is appended to that haproxy
`server` line (after `check` / `ssl verify none` added by `--check` / `--ssl`):

```sh
happyctrl add web --sub www --check /health \
  --server '127.0.0.1:8081 maxconn 100 alpn h2' \
  --server 'b=127.0.0.1:8082 weight 50 inter 2s fall 3 rise 2'
happyctrl server-opts web s1 maxconn 200        # change later (no options = clear)
```

renders

```
        server s1 127.0.0.1:8081 check maxconn 100 alpn h2
        server b 127.0.0.1:8082 check weight 50 inter 2s fall 3 rise 2
```

Options are free-form and checked by `haproxy -c` on apply, so a typo never
reaches the live config. For options shared by all servers of a service use
`--extra 'default-server maxconn 100'`. Server names come from `name=` or
default to s1, s2, ... (see `happyctrl ls`). Note: `alpn` only takes effect
together with `ssl` (`--ssl`); for cleartext HTTP/2 to a backend use `proto h2`.

## Subdomains

Instead of full hostnames, give a domain once and add services by subdomain.
Subdomains can be several levels deep; `@` means the domain itself.

```sh
happyctrl domain example.org                  # or: happyctrl init --domain example.org
happyctrl add jellyfin --sub jellyfin --sub media.home --server 127.0.0.1:8096
#   -> jellyfin.example.org, media.home.example.org
happyctrl add root --sub @ --server 127.0.0.1:8080       # -> example.org
happyctrl add nas  --sub nas --domain lan.internal ...   # per-service suffix
happyctrl add ext  --host git.example.org ...            # full name, no suffix
```

Subdomains are stored relative to the domain (`settings.domain`, or the
service's `domain`) and expanded when rendering, so `happyctrl domain new.tld`
moves every relative service at once. Host conflicts are checked on the
expanded names. In the template, `.Hosts` is always the full list.

## ACLs

Named ACLs are written into the `https` frontend as `acl <name> <rule>`, and a
service's ACLs are appended to its host match on the `use_backend` line, so
**all** of them must be true for the request to reach the service. Anything
else falls through to `default_backend` (404 by default).

```sh
happyctrl acl add lan src 192.168.0.0/16 10.0.0.0/8
happyctrl acl add lan src 127.0.0.1            # second rule: OR'ed with the first
happyctrl acl add curl 'req.hdr(user-agent)' -m sub -i curl
happyctrl acl attach nas lan                    # or: add nas ... --acl lan
happyctrl acl attach nas '!curl'                # ! negates
happyctrl acl ls
```

renders

```
    acl curl req.hdr(user-agent) -m sub -i curl
    acl lan src 192.168.0.0/16 10.0.0.0/8
    acl lan src 127.0.0.1
    use_backend be_nas if { var(txn.host) -m str nas.home.lan } lan !curl
```

The rule is everything after NAME, kept verbatim (only `--replace` and
`--apply` are treated as flags), and is checked by `haproxy -c` on apply.
HAProxy's predefined ACLs (`LOCALHOST`, `METH_GET`, ...) can be attached
without defining them. An ACL in use cannot be removed.

ACLs apply per service, i.e. to all of its hostnames. To guard only some
hostnames, put them in a separate service.

## Configuration (`happyctrl.json`)

Only how happyctrl installs the config, plus the ACL and service lists:
`settings.output_path` (default `/etc/haproxy/haproxy.cfg`),
`settings.validate_cmd` (`{file}` = candidate path, default
`haproxy -c -f {file}`), `settings.reload_cmd` (default
`systemctl reload haproxy`; empty = no reload), `acls` and `services`.

## Template

All HAProxy settings — bind ports, TLS certificates, maxconn, timeouts, stats,
default backend — live in `haproxy.cfg.tmpl` in the config dir. Edit it
directly (the built-in default is used if it is missing). happyctrl only fills
in the per-service parts.

Template data: `.ACLs` (each with `.Name`, `.Rules`), `.Services` (each with `.Name`, `.Hosts`, `.Servers`
(`.Name`, `.Addr`, `.Options`), `.HealthCheck`, `.ServerSSL`, `.Balance`, `.Extra`, `.ACLs`) and
`.TemplatePath`; helper `join`.

## Unix sockets

`--server /run/app.sock` (or `unix@/run/app.sock`) proxies to a unix socket.
The path must be absolute, and the user HAProxy runs as needs read/write
access to the socket. If you enable `chroot` in the template, the path is
resolved inside the chroot.
