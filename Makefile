# happyctrl — build, package and ship
#
#   make                      vet + test + build ./happyctrl
#   make dist                 release tarballs + SHA256SUMS in dist/
#   make install              install locally (PREFIX=/usr/local, DESTDIR=)
#   make ship   HOST=me@srv   build for the server's arch, install the binary there
#   make deploy HOST=me@srv   ship + push templates/haproxy.cfg.tmpl + init + apply
#
# The server needs ssh access and sudo. HAProxy runs on the server's host.

BINARY  := happyctrl
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
ARCHES  ?= amd64 arm64
DIST    := dist
GO      ?= go
LDFLAGS := -s -w -X main.version=$(VERSION)
GOBUILD := CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)'

PREFIX  ?= /usr/local
DESTDIR ?=

# Remote side (ship / deploy)
HOST       ?=
SSH        ?= ssh
SCP        ?= scp
SUDO       ?= sudo
REMOTE_BIN ?= /usr/local/bin
REMOTE_DIR ?= /etc/happyctrl
TEMPLATE   := templates/haproxy.cfg.tmpl

SHELL       := bash
.SHELLFLAGS := -eu -o pipefail -c
.ONESHELL:
.DEFAULT_GOAL := all

.PHONY: all build check vet test dist clean install uninstall ship deploy guard-host help

all: check build

help:
	@sed -n '3,10p' $(firstword $(MAKEFILE_LIST)) | sed 's/^# \{0,1\}//'

build:
	$(GOBUILD) -o $(BINARY) .

vet:
	@unformatted=$$(gofmt -l .)
	if [ -n "$$unformatted" ]; then echo "gofmt needed: $$unformatted" >&2; exit 1; fi
	$(GO) vet ./...

test:
	$(GO) test ./...

check: vet test

# ---- packaging -------------------------------------------------------------

# dist/happyctrl-<version>-linux-<arch>.tar.gz containing
#   happyctrl-<version>-linux-<arch>/{happyctrl,haproxy.cfg.tmpl,README.md}
dist: check
	rm -rf $(DIST) && mkdir -p $(DIST)
	for arch in $(ARCHES); do
	  name=$(BINARY)-$(VERSION)-linux-$$arch
	  mkdir -p $(DIST)/$$name
	  GOOS=linux GOARCH=$$arch $(GOBUILD) -o $(DIST)/$$name/$(BINARY) .
	  cp $(TEMPLATE) README.md $(DIST)/$$name/
	  tar -C $(DIST) --sort=name --owner=0 --group=0 --numeric-owner -czf $(DIST)/$$name.tar.gz $$name
	  rm -rf $(DIST)/$$name
	  echo "packaged $(DIST)/$$name.tar.gz"
	done
	cd $(DIST) && sha256sum *.tar.gz > SHA256SUMS && cat SHA256SUMS

clean:
	rm -rf $(DIST) $(BINARY)

install: build
	install -D -m 0755 $(BINARY) $(DESTDIR)$(PREFIX)/bin/$(BINARY)

uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/$(BINARY)

# ---- shipping --------------------------------------------------------------

guard-host:
	@if [ -z "$(HOST)" ]; then echo "set HOST, e.g. make $(MAKECMDGOALS) HOST=me@server" >&2; exit 1; fi

# Builds $(DIST)/remote/happyctrl for the server's CPU architecture.
define remote_build
arch=$$($(SSH) $(HOST) uname -m)
case "$$arch" in
  x86_64)        goarch=amd64 goarm= ;;
  aarch64|arm64) goarch=arm64 goarm= ;;
  armv7l)        goarch=arm   goarm=7 ;;
  armv6l)        goarch=arm   goarm=6 ;;
  *) echo "unsupported remote architecture: $$arch" >&2; exit 1 ;;
esac
echo "==> building $(VERSION) for $(HOST) ($$arch)"
mkdir -p $(DIST)/remote
GOOS=linux GOARCH=$$goarch GOARM=$$goarm $(GOBUILD) -o $(DIST)/remote/$(BINARY) .
tmp=$$($(SSH) $(HOST) mktemp -d)
endef

# Install only the binary on HOST.
ship: guard-host check
	$(remote_build)
	$(SCP) -q $(DIST)/remote/$(BINARY) "$(HOST):$$tmp/"
	echo "==> installing to $(HOST):$(REMOTE_BIN)/$(BINARY)"
	$(SSH) -t $(HOST) "set -e; trap 'rm -rf $$tmp' EXIT; \
	  $(SUDO) install -m 0755 $$tmp/$(BINARY) $(REMOTE_BIN)/$(BINARY); \
	  $(REMOTE_BIN)/$(BINARY) version"

# Binary + template, then init (keeps existing files) and apply (validates with
# haproxy -c before installing; the previous template is kept as .bak).
deploy: guard-host check
	$(remote_build)
	$(SCP) -q $(DIST)/remote/$(BINARY) $(TEMPLATE) "$(HOST):$$tmp/"
	echo "==> deploying to $(HOST) (binary $(REMOTE_BIN), config $(REMOTE_DIR))"
	$(SSH) -t $(HOST) "set -e; trap 'rm -rf $$tmp' EXIT; \
	  $(SUDO) install -m 0755 $$tmp/$(BINARY) $(REMOTE_BIN)/$(BINARY); \
	  $(SUDO) install -d -m 0755 $(REMOTE_DIR); \
	  if [ -f $(REMOTE_DIR)/haproxy.cfg.tmpl ]; then $(SUDO) cp -p $(REMOTE_DIR)/haproxy.cfg.tmpl $(REMOTE_DIR)/haproxy.cfg.tmpl.bak; fi; \
	  $(SUDO) install -m 0644 $$tmp/haproxy.cfg.tmpl $(REMOTE_DIR)/haproxy.cfg.tmpl; \
	  $(SUDO) $(REMOTE_BIN)/$(BINARY) --dir $(REMOTE_DIR) init; \
	  $(SUDO) $(REMOTE_BIN)/$(BINARY) --dir $(REMOTE_DIR) apply"
