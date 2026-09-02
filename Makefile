# cfrproxy build + deploy.
#
# The service (~/.config/systemd/user/cfrproxy.service) execs the binary at
# the root of this checkout. Never build straight onto that path while the
# service runs: INCIDENT-002 (timeline.md) was a truncated live executable.
# `make build` writes to a temp file and renames; `make deploy` also keeps
# $(KEEP) dated rollback copies and refuses to finish until /health, /admin/
# and /api/version all agree the new binary is the one serving.

BIN      := cfrproxy
PREFIX   ?= $(HOME)/.local/bin
SERVICE  ?= cfrproxy
ADDR     ?= http://127.0.0.1:8420
KEEP     ?= 2

COMMIT   := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DIRTY    := $(shell git diff --quiet -- . ':!timeline.md' 2>/dev/null || echo -dirty)
VERSION  := $(shell git describe --tags --always 2>/dev/null || echo dev)$(DIRTY)
DATE     := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -X main.version=$(VERSION) -X main.commit=$(COMMIT)$(DIRTY) -X main.buildDate=$(DATE)
STAMP    := $(shell date +%Y%m%d-%H%M%S)

.PHONY: help test race build install deploy rollback health version clean

help:
	@echo "make test      go vet + go test"
	@echo "make race      go test -race (slower)"
	@echo "make build     build ./$(BIN) safely (temp file + rename)"
	@echo "make install   copy ./$(BIN) to $(PREFIX)/$(BIN) (atomic rename)"
	@echo "make deploy    test, back up, build, install, restart $(SERVICE), verify"
	@echo "make rollback  restore the newest $(BIN).bak-* and restart"
	@echo "make health    check the running service"
	@echo "make clean     prune rollback copies beyond KEEP=$(KEEP), drop temp files"

test:
	go vet ./...
	go test ./...

race:
	go test -race ./...

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN).tmp .
	mv -f $(BIN).tmp $(BIN)

install: $(BIN)
	mkdir -p $(PREFIX)
	install -m755 $(BIN) $(PREFIX)/.$(BIN).new
	mv -f $(PREFIX)/.$(BIN).new $(PREFIX)/$(BIN)

deploy: test
	@if [ -x $(BIN) ]; then cp -p $(BIN) $(BIN).bak-$(STAMP); echo "rollback copy: $(BIN).bak-$(STAMP)"; fi
	$(MAKE) --no-print-directory build install
	systemctl --user restart $(SERVICE)
	$(MAKE) --no-print-directory health EXPECT=$(COMMIT)$(DIRTY)
	@ls -t $(BIN).bak-* 2>/dev/null | tail -n +$$(( $(KEEP) + 1 )) | xargs -r rm -f
	@echo "deployed $(VERSION) ($(COMMIT)$(DIRTY)) at $(DATE)"

rollback:
	@prev=$$(ls -t $(BIN).bak-* 2>/dev/null | head -1); \
	if [ -z "$$prev" ]; then echo "no $(BIN).bak-* to roll back to"; exit 1; fi; \
	echo "rolling back to $$prev"; cp -p $$prev $(BIN).tmp && mv -f $(BIN).tmp $(BIN)
	$(MAKE) --no-print-directory install
	systemctl --user restart $(SERVICE)
	$(MAKE) --no-print-directory health

# EXPECT=<commit> additionally requires /api/version to report that commit.
health:
	@for i in $$(seq 1 40); do \
	  if curl -fsS -m 2 $(ADDR)/health >/dev/null 2>&1; then break; fi; sleep 0.5; \
	  if [ $$i -eq 40 ]; then echo "FAIL: $(ADDR)/health not up after 20s"; systemctl --user status $(SERVICE) --no-pager | head -12; journalctl --user -u $(SERVICE) -n 20 --no-pager; exit 1; fi; \
	done
	@code=$$(curl -s -o /dev/null -w '%{http_code}' -m 5 $(ADDR)/admin/); \
	if [ "$$code" != "401" ]; then echo "FAIL: /admin/ returned $$code, expected 401"; exit 1; fi
	@ver=$$(curl -fsS -m 5 $(ADDR)/api/version); echo "running: $$ver"; \
	if [ -n "$(EXPECT)" ] && ! echo "$$ver" | grep -q '"commit":"$(EXPECT)"'; then echo "FAIL: service is not running commit $(EXPECT)"; exit 1; fi
	@systemctl --user show $(SERVICE) -p ActiveState -p MainPID --no-pager | tr '\n' ' '; echo

version:
	@echo "$(VERSION) $(COMMIT)$(DIRTY) $(DATE)"

clean:
	rm -f $(BIN).tmp
	@ls -t $(BIN).bak-* 2>/dev/null | tail -n +$$(( $(KEEP) + 1 )) | xargs -r rm -fv
