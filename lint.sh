#!/usr/bin/env sh

set -xue

if [ -x ../ay/ay ]; then
    ../ay/ay dev refac lint ./*.go
fi

gofmt -l *.go | { ! grep .; }
go vet ./...
./build logovo
