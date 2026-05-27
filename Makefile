VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# macOS launchd deploy (single-user, foreground service on :8080):
#   PREFIX        — where the binary lands; default /opt/homebrew/bin (Apple
#                   Silicon Homebrew). Override with PREFIX=/usr/local/bin etc.
#   LAUNCH_LABEL  — launchd job label; the plist filename derives from it.
#   PLIST_PATH    — full path to the LaunchAgent plist.
#   DATA_DIR / LOG_DIR — runtime state; ~/.config/shipd by default so the repo
#                   directory stays pure source code.
PREFIX       ?= /opt/homebrew/bin
LAUNCH_LABEL ?= lol.jiangtao.shipd
PLIST_PATH   ?= $(HOME)/Library/LaunchAgents/$(LAUNCH_LABEL).plist
DATA_DIR     ?= $(HOME)/.config/shipd/data
LOG_DIR      ?= $(HOME)/.config/shipd/logs
LISTEN_ADDR  ?= :8080
PUBLIC_BASE  ?= https://shipd.jiangtao.lol

.PHONY: build run tidy test clean docker install restart uninstall logs

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o shipd ./cmd/shipd

run: build
	./shipd serve --data-dir ./data

tidy:
	go mod tidy

test:
	go test ./...

clean:
	rm -f shipd
	rm -rf dist
	@echo "(data dir at $(DATA_DIR) preserved — remove manually if you really mean it)"

docker:
	docker build -t shipd:$(VERSION) .

# install: drop the binary into PREFIX, install/refresh the LaunchAgent plist,
# create state/log dirs, and load (or reload) the service. Safe to re-run for
# upgrades — uses launchctl bootout+bootstrap so config changes take effect.
install: build
	@install -d "$(dir $(PREFIX))" "$(DATA_DIR)" "$(LOG_DIR)" "$(dir $(PLIST_PATH))"
	install -m 0755 shipd "$(PREFIX)/shipd"
	@echo "→ installed $(PREFIX)/shipd ($(VERSION))"
	@PREFIX="$(PREFIX)" LAUNCH_LABEL="$(LAUNCH_LABEL)" DATA_DIR="$(DATA_DIR)" \
	 LOG_DIR="$(LOG_DIR)" LISTEN_ADDR="$(LISTEN_ADDR)" PUBLIC_BASE="$(PUBLIC_BASE)" \
	 ./deploy/render-plist.sh > "$(PLIST_PATH)"
	@echo "→ wrote $(PLIST_PATH)"
	@launchctl bootout gui/$$(id -u) "$(PLIST_PATH)" 2>/dev/null || true
	@launchctl bootstrap gui/$$(id -u) "$(PLIST_PATH)"
	@echo "→ launchctl bootstrapped $(LAUNCH_LABEL)"
	@sleep 1
	@launchctl print gui/$$(id -u)/$(LAUNCH_LABEL) 2>/dev/null | awk '/state|pid/ {print "  "$$0}' || echo "(launchctl print: no info yet)"

# restart: pick up a new binary without re-rendering the plist. Use this for
# code-only upgrades; `make install` is the right call when args/env change.
restart:
	launchctl kickstart -k "gui/$$(id -u)/$(LAUNCH_LABEL)"
	@sleep 1
	@launchctl print gui/$$(id -u)/$(LAUNCH_LABEL) 2>/dev/null | awk '/state|pid/ {print "  "$$0}' || true

# uninstall: tear down the service. Deliberately does NOT delete DATA_DIR —
# the SQLite catalog and IPA blobs are user data, not infrastructure.
uninstall:
	-launchctl bootout gui/$$(id -u) "$(PLIST_PATH)" 2>/dev/null
	-rm -f "$(PLIST_PATH)"
	-rm -f "$(PREFIX)/shipd"
	@echo "(data dir at $(DATA_DIR) preserved — remove manually if you really mean it)"

logs:
	@echo "--- $(LOG_DIR)/shipd.log ---" && tail -n 50 "$(LOG_DIR)/shipd.log" 2>/dev/null || echo "(no log yet)"
	@echo "--- $(LOG_DIR)/shipd.error.log ---" && tail -n 50 "$(LOG_DIR)/shipd.error.log" 2>/dev/null || echo "(no error log yet)"
