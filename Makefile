BIN := bin/srvimit
CAT := bin/catalogup
GO  ?= go
VER ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDF := -s -w -X main.version=$(VER)

.PHONY: all build build-catalog check fmt vet test lint image image-multiarch cross up up-catalog down list serve catalog catalog-up record verify smoke dist clean

all: check build

build:
	$(GO) build -trimpath -ldflags "$(LDF)" -o $(BIN) ./cmd/srvimit

build-catalog:
	$(GO) build -trimpath -ldflags "-s -w" -o $(CAT) ./cmd/catalogup

# Everything CI runs, in the same order.
check: fmt vet test
	$(GO) test ./cmd/catalogup/ -run Catalog -v

fmt:
	@out=$$(gofmt -l ./cmd ./internal); if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

test:
	$(GO) test -race ./...

lint: fmt vet

# Static binaries for the release targets.
cross:
	mkdir -p dist
	for t in linux/amd64 linux/arm64 darwin/arm64; do \
		os=$${t%/*}; arch=$${t#*/}; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath \
			-ldflags "$(LDF)" -o dist/srvimit-$$os-$$arch ./cmd/srvimit; \
	done
	ls -lh dist/

# Что доступно для -service.
list: build
	./$(BIN) list

serve: build
	./$(BIN) serve -all -base-port 8000

# Снять снапшоты всех сервисов каталога через Docker (долго: тянет образы).
catalog: build-catalog
	./$(CAT) -all -capture -timeout 420s

# То же, но контейнеры остаются поднятыми (нужно для verify).
catalog-up: build-catalog
	./$(CAT) -all -capture -keep -timeout 420s

record: build
	./$(BIN) record -config services.yaml

verify: build
	./$(BIN) verify -config services.yaml

# Полная приёмка: юнит-тесты + сравнение с живыми апстримами + рендер в Chromium.
smoke: test verify
	./tools/browser-smoke.sh http://127.0.0.1:8080/login fixtures/grafana "Welcome to Grafana"

# Готовый к деплою бандл: бинарь + снапшоты, которые есть локально.
dist: build
	rm -rf dist && mkdir -p dist/bin
	cp $(BIN) dist/bin/
	if [ -d fixtures ]; then cp -r fixtures dist/; fi
	cp services.yaml README.md README-RU.md LICENSE dist/
	cp -r deploy tools catalog dist/
	tar -czf srvimit-$(VER).tar.gz -C dist .

clean:
	rm -rf bin dist *.tar.gz