GO ?= go
FLAGS = -mod=vendor -trimpath

.PHONY: build
build:
	$(GO) build $(FLAGS) -o build/bot-core ./cmd/bot-core
