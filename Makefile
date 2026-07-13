# Load variables from .env if it exists, and export them to the child process.
ifneq (,$(wildcard .env))
include .env
export
endif

.PHONY: run run-once build vet fmt tidy help

## run: sync now, then loop on the ticker every SYNC_INTERVAL_HOURS (reads .env)
run:
	go run .

## run-once: sync a single time and exit (reads .env)
run-once:
	go run . -once

## build: compile check
build:
	go build ./...

## vet: static analysis
vet:
	go vet ./...

## fmt: format all Go files
fmt:
	go fmt ./...

## tidy: sync go.mod
tidy:
	go mod tidy

## help: list available targets
help:
	@grep -hE '^## ' $(MAKEFILE_LIST) | sed 's/## //'
