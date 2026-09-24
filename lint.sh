#!/usr/bin/env sh

set -xue

gofmt -l *.go | { ! grep .; }
go vet ./...
./build logovo
