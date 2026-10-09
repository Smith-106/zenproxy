BINARY := zenproxy
PKG := ./cmd/zenproxy
GO ?= go

.PHONY: all build build-linux run test vet fmt tidy clean smoke

all: vet test build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o build/$(BINARY) $(PKG)

# 目标服务器是 linux/amd64（Oracle Ubuntu）
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags="-s -w" -o build/$(BINARY)-linux-amd64 $(PKG)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags="-s -w" -o build/$(BINARY)-linux-arm64 $(PKG)

# 本地起一个实例，用 config.local.yaml（自己从 config.example.yaml 复制）
run: build
	./build/$(BINARY) -config config.local.yaml

test:
	$(GO) test ./... -count=1

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy

clean:
	rm -rf build

# 端到端冒烟：起 mock 上游 + mock 出口，跑完自动清理
smoke:
	$(GO) test ./internal/... -run TestEndToEnd -v -count=1
