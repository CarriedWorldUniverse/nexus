VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/CarriedWorldUniverse/nexus/internal/version.Version=$(VERSION)

# Binaries shipped from this repo. Mapped from cmd-dir → bin name. All
# share the same version package and ldflags.
BINS := nexus agentfunnel aspect nexus-comms-mcp nexus-imap-mcp nexus-jira-mcp nexus-skills-mcp nexus-watch outpost

CMD_nexus           := ./nexus/cmd/nexus
CMD_agentfunnel     := ./runtime/cmd/agentfunnel
CMD_aspect          := ./runtime/cmd/aspect
CMD_nexus-comms-mcp := ./runtime/cmd/nexus-comms-mcp
CMD_nexus-imap-mcp  := ./runtime/cmd/nexus-imap-mcp
CMD_nexus-jira-mcp  := ./runtime/cmd/nexus-jira-mcp
CMD_nexus-skills-mcp := ./runtime/cmd/nexus-skills-mcp
CMD_nexus-watch     := ./runtime/cmd/nexus-watch
CMD_outpost         := ./nexus/cmd/outpost

# Path to a llama.cpp checkout for the optional ctxmap-enabled aspect build.
LLAMA_CPP ?= $(HOME)/src/llama.cpp

.PHONY: build $(BINS) test vet version clean all vendor-llama aspect-ctxmap

all: $(BINS)

build: $(BINS)

$(BINS):
	@mkdir -p bin
	go build -ldflags '$(LDFLAGS)' -o bin/$@ $(CMD_$@)

bin/%:
	@mkdir -p bin
	go build -ldflags '$(LDFLAGS)' -o $@ $(CMD_$*)

test:
	go test -race ./...

vet:
	go vet ./...

version:
	@echo $(VERSION)

clean:
	rm -rf bin/

# --- optional: ctxmap working-memory-enabled aspect binary ---------------------
# The default aspect build carries no cgo. `aspect-ctxmap` builds it WITH the
# ctxmap_llama tag, linking the llama.cpp libs the in-harness extractor needs.
# Requires a llama.cpp checkout (LLAMA_CPP); build its shared libs first with
# `make vendor-llama`. At runtime the feature is still gated on CTXMAP_ENABLED.
vendor-llama:
	cd $(LLAMA_CPP) && cmake -B build -DBUILD_SHARED_LIBS=ON -DLLAMA_BUILD_TESTS=OFF -DLLAMA_BUILD_EXAMPLES=OFF -DLLAMA_BUILD_SERVER=OFF -DCMAKE_BUILD_TYPE=Release && cmake --build build -j $$(nproc) --target llama

aspect-ctxmap:
	@mkdir -p bin
	CGO_ENABLED=1 \
	CGO_CFLAGS="-I$(LLAMA_CPP)/include -I$(LLAMA_CPP)/ggml/include" \
	CGO_LDFLAGS="-L$(LLAMA_CPP)/build/bin -Wl,-rpath,$(LLAMA_CPP)/build/bin" \
	go build -tags ctxmap_llama -ldflags '$(LDFLAGS)' -o bin/aspect-ctxmap $(CMD_aspect)
