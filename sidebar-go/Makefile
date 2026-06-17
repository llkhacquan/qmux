INSTALL_DIR := $(HOME)/.local/bin

SCRIPTS := sidebar-focus-nav sidebar-focus-send sidebar-switch-last

.PHONY: build install install-scripts test check lint clean

build:
	go build -o .bin/sidebar-go .

# DO NOT strip these binaries. A stripped static Go binary trips EDR malware
# heuristics and gets SIGKILL'd on every exec (exit 137). The unstripped
# (symbol-rich) binary is the safe one: EDR scans it instead of killing it.
install: build install-scripts
	mkdir -p $(INSTALL_DIR)
	cp .bin/sidebar-go $(INSTALL_DIR)/sidebar-go.tmp
	codesign -s - $(INSTALL_DIR)/sidebar-go.tmp 2>/dev/null || true
	mv $(INSTALL_DIR)/sidebar-go.tmp $(INSTALL_DIR)/sidebar-go

install-scripts:
	mkdir -p $(INSTALL_DIR)
	@for s in $(SCRIPTS); do \
		ln -sf $(CURDIR)/scripts/$$s $(INSTALL_DIR)/$$s; \
	done

test:
	go test ./...

check: lint
	go test -race ./...

lint:
	golangci-lint run ./...

clean:
	rm -rf .bin
