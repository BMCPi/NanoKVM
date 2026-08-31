# Makefile for NanoKVM BMC Project

# Configuration
IMAGE_NAME := nanokvm-builder
UID := $(shell id -u)
GID := $(shell id -g)
PWD := $(shell pwd)

# Deploy configuration (override on the command line: make deploy KVM_HOST=...)
KVM_HOST ?= 10.0.150.207
KVM_SCHEME ?= http
KVM_USER ?= admin
KVM_PASS ?= admin
# Transport obfuscation key, extracted from its single source of truth so a
# key rotation cannot silently break deploys (guarded in the deploy recipe).
KVM_SECRET := $(shell sed -n 's/^const SecretKey = "\(.*\)"$$/\1/p' pkg/utils/encrypt.go)

.PHONY: help templ app all clean snapshot deploy sensord

# Default target
all: app

# Help target
help:
	@echo "NanoKVM BMC Build System"
	@echo ""
	@echo "Available targets:"
	@echo "  help          - Show this help message"
	@echo "  templ         - Generate Go code from templ templates"
	@echo "  app           - Build Go application server (runs templ generate first)"
	@echo "  all           - Build app (default)"
	@echo "  clean         - Clean build artifacts"
	@echo "  snapshot      - Build snapshot release with goreleaser (no publish)"
	@echo "  deploy        - Upload built server to a device via offline update (KVM_HOST/KVM_USER/KVM_PASS)"
	@echo "  sensord       - Build bmc-sensord for the managed host (arm64), not the BMC"
	@echo ""
	@echo "Prerequisites:"
	@echo "  - Docker must be installed and running"
	@echo "  - Must not run as root user"

# Generate Go code from templ templates
format:
	@echo "Formatting code..."
	golangci-lint fmt ./...

generate:
	@echo "Generating code..."
	go generate ./...

# Version stamping, mirroring .goreleaser.yaml's ldflags so a locally built
# binary reports its real version instead of "dev".
VERSION := $(shell git describe --tags --always | sed 's/^v//')
COMMIT := $(shell git rev-parse HEAD)
DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

dist/server/NanoKVM-Server:
	@echo "Creating output directory..."
	@mkdir -p dist/server
	@go mod tidy
	@CGO_ENABLED=0 GOOS=linux GOARCH=riscv64 go build -trimpath -ldflags "$(LDFLAGS)" -o ./dist/server/NanoKVM-Server ./cmd/server
	@upx -q -v ./dist/server/NanoKVM-Server

dist/rpiboot/rpiboot:
	@mkdir -p dist/rpiboot
	@CGO_ENABLED=0 GOOS=linux GOARCH=riscv64 go build -trimpath -ldflags "-s -w" -o ./dist/rpiboot/rpiboot ./cmd/rpiboot

# Build Go application (generates first)
app: generate clean format
	@echo "Building app..."
	$(MAKE) dist/server/NanoKVM-Server

# Clean build artifacts
clean:
	@echo "Cleaning build artifacts..."
	@if [ -f dist/server/NanoKVM-Server ]; then \
		rm -f dist/server/NanoKVM-Server; \
		echo "Removed NanoKVM-Server"; \
	fi
	@echo "Clean completed."

# bmc-sensord runs on the managed Raspberry Pi, not on the BMC: it talks to
# OP-TEE through /dev/teeN, which only exists on the host. That is why it is
# built for arm64 here and is deliberately absent from the deploy package
# below, which replaces the BMC's riscv64 app directory wholesale.
dist/host/bmc-sensord:
	@mkdir -p dist/host
	@CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o ./dist/host/bmc-sensord ./cmd/bmc-sensord

sensord: dist/host/bmc-sensord
	@echo "Built dist/host/bmc-sensord (linux/arm64)"

# Build snapshot release using goreleaser (no publish)
snapshot:
	@goreleaser release --snapshot --clean --skip=publish

# Upload the built server to a device through the offline-update API.
# The package layout must match the goreleaser archive (.goreleaser.yaml):
# server/NanoKVM-Server + system/usr/bin/rpiboot + version. The updater
# replaces the whole app dir, so anything missing here is removed on-device.
# (No chmod needed — the installer runs ChmodRecursively after extraction.)
deploy: dist/server/NanoKVM-Server dist/rpiboot/rpiboot
	@test -n '$(KVM_SECRET)' || { echo "KVM_SECRET extraction from pkg/utils/encrypt.go failed"; exit 1; }
	@echo "Packaging update..."
	@rm -rf dist/deploy
	@mkdir -p dist/deploy/pkg/server dist/deploy/pkg/system/usr/bin
	@cp dist/server/NanoKVM-Server dist/deploy/pkg/server/NanoKVM-Server
	@cp dist/rpiboot/rpiboot dist/deploy/pkg/system/usr/bin/rpiboot
	@printf '%s' '$(VERSION)' > dist/deploy/pkg/version
	@tar -czf dist/deploy/update.tar.gz -C dist/deploy/pkg server system version
	@echo "Deploying $(VERSION) to $(KVM_HOST)..."
	@PW=$$(printf '%s' '$(KVM_PASS)' \
		| openssl enc -aes-256-cbc -md md5 -pass 'pass:$(KVM_SECRET)' -base64 -A 2>/dev/null \
		| sed -e 's/+/%2B/g' -e 's|/|%2F|g' -e 's/=/%3D/g'); \
	TOKEN=$$(curl -ksS -m 15 -X POST "$(KVM_SCHEME)://$(KVM_HOST)/api/auth/login" \
		-H 'Content-Type: application/json' \
		-d "{\"username\":\"$(KVM_USER)\",\"password\":\"$$PW\"}" \
		| sed -n 's/.*"token":"\([^"]*\)".*/\1/p'); \
	if [ -z "$$TOKEN" ]; then echo "Login to $(KVM_HOST) failed"; exit 1; fi; \
	RSP=$$(curl -ksS -m 300 -X POST "$(KVM_SCHEME)://$(KVM_HOST)/api/application/update/offline" \
		-H "Cookie: nano-kvm-token=$$TOKEN" \
		-F "file=@dist/deploy/update.tar.gz;type=application/gzip"); \
	echo "$$RSP"; \
	case "$$RSP" in *'"code":0,'*) ;; *) echo "Update failed"; exit 1;; esac; \
	echo "Waiting for service to restart..."; \
	i=0; while [ $$i -lt 24 ]; do \
		VER=$$(curl -ksS -m 5 "$(KVM_SCHEME)://$(KVM_HOST)/api/application/version" \
			-H "Cookie: nano-kvm-token=$$TOKEN" 2>/dev/null \
			| sed -n 's/.*"current":"\([^"]*\)".*/\1/p'); \
		if [ -n "$$VER" ]; then echo "Device is back up, running version $$VER"; exit 0; fi; \
		i=$$((i + 1)); sleep 5; \
	done; \
	echo "Device did not come back within 120s"; exit 1
