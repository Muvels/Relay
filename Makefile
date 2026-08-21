VERSION := 0.1.0
LDFLAGS := -ldflags "-X main.version=$(VERSION)"
PLATFORMS := linux/amd64 linux/arm64 darwin/arm64 darwin/amd64

.PHONY: build test release stage clean dashboard

# The binary embeds the built web dashboard, so build it first.
dashboard:
	cd dashboard && pnpm install --silent && pnpm build

build: dashboard
	cd relayd && go build $(LDFLAGS) -o bin/relayd ./cmd/relayd

test:
	cd relayd && go vet ./... && go test ./...
	PATH="$(PWD)/.venv/bin:$(PATH)" .venv/bin/python -m pytest sdk/tests/

# Cross-compile relayd for every fleet platform into relayd/bin/.
release: dashboard
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		echo "→ relayd-$$os-$$arch"; \
		cd relayd && CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build $(LDFLAGS) -o bin/relayd-$$os-$$arch ./cmd/relayd && cd ..; \
	done

# Stage release binaries + installer into a running local server's data dir
# so `curl <server>/install.sh | sh` works on fleet machines.
stage: release
	mkdir -p $(HOME)/.relay/server/binaries
	cp relayd/bin/relayd-* $(HOME)/.relay/server/binaries/
	cp installer/install.sh $(HOME)/.relay/server/binaries/

clean:
	rm -rf relayd/bin
