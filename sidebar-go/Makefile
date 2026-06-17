INSTALL_DIR := $(HOME)/.local/bin

.PHONY: build install test check lint clean

build:
	go build -o .bin/sidebar-go .

# DO NOT strip these binaries. A stripped static Go binary trips EDR malware
# heuristics and gets SIGKILL'd on every exec (exit 137). The unstripped
# (symbol-rich) binary is the safe one: EDR scans it instead of killing it.
install: build
	mkdir -p $(INSTALL_DIR)
	cp .bin/sidebar-go $(INSTALL_DIR)/sidebar-go.tmp
	codesign -s - $(INSTALL_DIR)/sidebar-go.tmp 2>/dev/null || true
	mv $(INSTALL_DIR)/sidebar-go.tmp $(INSTALL_DIR)/sidebar-go

test:
	go test ./...

check: lint
	go test -race ./...

lint:
	golangci-lint run ./...

clean:
	rm -rf .bin
