SHELL := /bin/bash
GO ?= go
BIN_DIR ?= bin

# OS detection for platform-specific defaults
UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
    STT_UDS_DEFAULT := /tmp/stt.sock
else
    STT_UDS_DEFAULT := /run/app/stt.sock
endif
STT_UDS_PATH ?= $(STT_UDS_DEFAULT)

SERVER_PKG := ./cmd/server
SERVER_BIN := $(BIN_DIR)/server

.PHONY: help server build test fmt vet tidy clean

help:
	@echo "Targets:"
	@echo "  make server   - Run the API server (go run)"
	@echo "  make build    - Build server binary to $(SERVER_BIN)"
	@echo "  make test     - Run unit tests"
	@echo "  make fmt      - Format code with go fmt"
	@echo "  make vet      - Static analysis with go vet"
	@echo "  make tidy     - Sync go.mod/go.sum"
	@echo "  make clean    - Remove build artifacts"
	@echo "  make smoke1   - Run smoke test script (requires server running)"
	@echo "  make smoke2   - Run barge-in smoke test (requires server running)"
	@echo "  make smoke2_real - Run real-VAD barge-in smoke test (requires VAD)"
	@echo "  make call        - Create session, start bot, open room in browser"
	@echo "  make health      - Check API server health (requires server running)"
	@echo "  make health-all  - Check all service health endpoints (sidecar, orch, llm, tts)"
	@echo "  make test-deepgram - Test Deepgram API key with silence (testdata/test.wav)"
	@echo "  make test-deepgram-speech - Test Deepgram with real speech (testdata/speech.wav)"
	@echo "  make test-azure-openai - Test Azure OpenAI chat completion"
	@echo "  make test-elevenlabs - Test ElevenLabs TTS (saves to testdata/tts-test.mp3)"
	@echo "  make test-llm-service - Test LLM gRPC service (requires grpcurl, service on :9092)"
	@echo "  make proto-go    - Generate all Go gRPC stubs"
	@echo "  make proto-py    - Generate all Python gRPC stubs into gateway/"
	@echo "  make sidecar     - Run STT sidecar (socket: $(STT_UDS_PATH))"
	@echo "  make sidecar-build - Build STT sidecar binary to $(BIN_DIR)/stt-sidecar"
	@echo "  make proto-go-gw - Generate Go stubs for GatewayControl"
	@echo "  make proto-py-gw - Generate Python stubs for GatewayControl"
	@echo "  make orch        - Run Orchestrator (go run)"
	@echo "  make orch-build  - Build Orchestrator binary to $(BIN_DIR)/orchestrator"
	@echo "  make proto-go-llm - Generate Go stubs for LLM"
	@echo "  make proto-go-tts - Generate Go stubs for TTS"
	@echo "  make llm         - Run LLM service (go run)"
	@echo "  make llm-build   - Build LLM binary to $(BIN_DIR)/llm"
	@echo "  make tts         - Run TTS service (go run)"
	@echo "  make tts-build   - Build TTS binary to $(BIN_DIR)/tts"

.PHONY: proto-go
proto-go:
	protoc -I proto --go_out=. --go_opt=module=yuzu/agent --go-grpc_out=. --go-grpc_opt=module=yuzu/agent proto/stt.proto proto/gateway_control.proto proto/llm.proto proto/tts.proto

.PHONY: proto-py
proto-py:
	python3 -m grpc_tools.protoc -I proto --python_out=gateway --grpc_python_out=gateway proto/stt.proto proto/gateway_control.proto proto/llm.proto proto/tts.proto
	python3 scripts/patch_proto_imports.py gateway

.PHONY: proto-go-gw
proto-go-gw:
	protoc -I proto --go_out=. --go_opt=module=yuzu/agent --go-grpc_out=. --go-grpc_opt=module=yuzu/agent proto/gateway_control.proto

.PHONY: proto-go-llm
proto-go-llm:
	protoc -I proto --go_out=. --go_opt=module=yuzu/agent --go-grpc_out=. --go-grpc_opt=module=yuzu/agent proto/llm.proto

.PHONY: proto-go-tts
proto-go-tts:
	protoc -I proto --go_out=. --go_opt=module=yuzu/agent --go-grpc_out=. --go-grpc_opt=module=yuzu/agent proto/tts.proto

.PHONY: proto-py-gw
proto-py-gw:
	python3 -m grpc_tools.protoc -I proto --python_out=gateway --grpc_python_out=gateway proto/gateway_control.proto
	python3 scripts/patch_proto_imports.py gateway

.PHONY: sidecar
sidecar:
	@set -a && source .env && set +a && $(GO) run ./cmd/stt-sidecar --uds $(STT_UDS_PATH)

.PHONY: sidecar-build
sidecar-build:
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/stt-sidecar ./cmd/stt-sidecar

.PHONY: orch
orch:
	@set -a && source .env && set +a && $(GO) run ./cmd/orchestrator

.PHONY: orch-build
orch-build:
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/orchestrator ./cmd/orchestrator

.PHONY: llm
llm:
	@set -a && source .env && set +a && $(GO) run ./cmd/llm

.PHONY: llm-build
llm-build:
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/llm ./cmd/llm

.PHONY: tts
tts:
	@set -a && source .env && set +a && $(GO) run ./cmd/tts

.PHONY: tts-build
tts-build:
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/tts ./cmd/tts

.PHONY: all-services
all-services:
	@echo "Starting services: sidecar, orchestrator, llm, tts (STT socket: $(STT_UDS_PATH))"
	@set -a && source .env && set +a && \
	$(GO) run ./cmd/stt-sidecar --uds $(STT_UDS_PATH) & \
	SIDECAR_PID=$$!; \
	$(GO) run ./cmd/orchestrator & ORCH_PID=$$!; \
	$(GO) run ./cmd/llm & LLM_PID=$$!; \
	$(GO) run ./cmd/tts & TTS_PID=$$!; \
	trap 'kill $$SIDECAR_PID $$ORCH_PID $$LLM_PID $$TTS_PID' EXIT; \
	wait

server:
	$(GO) run $(SERVER_PKG)

build:
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(SERVER_BIN) $(SERVER_PKG)

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

clean:
	rm -rf $(BIN_DIR)

.PHONY: smoke1
smoke1:
	bash scripts/smoke_part1.sh

.PHONY: smoke2
smoke2:
	bash scripts/smoke_part2.sh

.PHONY: smoke2_real
smoke2_real:
	bash scripts/smoke_part2_real.sh

.PHONY: call
call:
	@bash scripts/dev_call.sh

.PHONY: health
health:
	@curl -s http://localhost:$${PORT:-8080}/health | jq .

.PHONY: test-deepgram
test-deepgram:
	@DEEPGRAM_API_KEY=$$(grep '^DEEPGRAM_API_KEY=' .env | cut -d= -f2 | tr -d '"') && \
	curl -s -X POST "https://api.deepgram.com/v1/listen" \
		-H "Authorization: Token $$DEEPGRAM_API_KEY" \
		-H "Content-Type: audio/wav" \
		--data-binary @testdata/test.wav | jq .

.PHONY: test-deepgram-speech
test-deepgram-speech:
	@DEEPGRAM_API_KEY=$$(grep '^DEEPGRAM_API_KEY=' .env | cut -d= -f2 | tr -d '"') && \
	curl -s -X POST "https://api.deepgram.com/v1/listen" \
		-H "Authorization: Token $$DEEPGRAM_API_KEY" \
		-H "Content-Type: audio/wav" \
		--data-binary @testdata/speech.wav | jq -r '.results.channels[0].alternatives[0].transcript'

.PHONY: test-azure-openai
test-azure-openai:
	@ENDPOINT=$$(grep '^AZURE_OPENAI_ENDPOINT=' .env | cut -d= -f2 | tr -d '"') && \
	API_KEY=$$(grep '^AZURE_OPENAI_API_KEY=' .env | cut -d= -f2 | tr -d '"') && \
	DEPLOYMENT=$$(grep '^AZURE_OPENAI_DEPLOYMENT=' .env | cut -d= -f2 | tr -d '"') && \
	API_VERSION=$$(grep '^AZURE_OPENAI_API_VERSION=' .env | cut -d= -f2 | tr -d '"') && \
	curl -s -X POST "$${ENDPOINT}openai/deployments/$${DEPLOYMENT}/chat/completions?api-version=$${API_VERSION}" \
		-H "api-key: $$API_KEY" \
		-H "Content-Type: application/json" \
		-d '{"messages": [{"role": "user", "content": "Say hello in exactly 5 words"}], "max_tokens": 50}' | jq -r '.choices[0].message.content'

.PHONY: test-elevenlabs
test-elevenlabs:
	@API_KEY=$$(grep '^ELEVENLABS_API_KEY=' .env | cut -d= -f2 | tr -d '"') && \
	VOICE_ID=$$(grep '^ELEVENLABS_VOICE_ID=' .env | cut -d= -f2 | tr -d '"') && \
	curl -s -X POST "https://api.elevenlabs.io/v1/text-to-speech/$${VOICE_ID}" \
		-H "xi-api-key: $$API_KEY" \
		-H "Content-Type: application/json" \
		-d '{"text": "Hello, this is a test.", "model_id": "eleven_monolingual_v1"}' \
		-o testdata/tts-test.mp3 && \
	echo "Saved to testdata/tts-test.mp3 ($$(ls -lh testdata/tts-test.mp3 | awk '{print $$5}'))"

.PHONY: test-llm-service
test-llm-service:
	@DEPLOYMENT=$$(grep '^AZURE_OPENAI_DEPLOYMENT=' .env | cut -d= -f2 | tr -d '"') && \
	API_VERSION=$$(grep '^AZURE_OPENAI_API_VERSION=' .env | cut -d= -f2 | tr -d '"') && \
	echo '{"start":{"session_id":"test","request_id":"test-1","deployment":"'$$DEPLOYMENT'","api_version":"'$$API_VERSION'","messages":[{"role":"user","content":"Say hello in 5 words"}],"stream":true}}' | \
	grpcurl -plaintext -d @ -import-path proto -proto llm.proto localhost:9092 llm.v1.LLM/Session

.PHONY: health-all
health-all:
	@echo "STT Sidecar (8081):  $$(curl -s -o /dev/null -w '%{http_code}' http://localhost:8081/healthz) $$(curl -s http://localhost:8081/healthz)"
	@echo "Orchestrator (8082): $$(curl -s -o /dev/null -w '%{http_code}' http://localhost:8082/healthz) $$(curl -s http://localhost:8082/healthz)"
	@echo "LLM (8083):          $$(curl -s -o /dev/null -w '%{http_code}' http://localhost:8083/healthz) $$(curl -s http://localhost:8083/healthz)"
	@echo "TTS (8084):          $$(curl -s -o /dev/null -w '%{http_code}' http://localhost:8084/healthz) $$(curl -s http://localhost:8084/healthz)"
