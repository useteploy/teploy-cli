.PHONY: build test lint vet clean quickstart release-verify release-record release-smoke

build:
	go build -o teploy ./cmd/teploy

test:
	go test ./... -v

lint:
	golangci-lint run ./...

vet:
	go vet ./...

clean:
	rm -f teploy

# Executable quickstart (C09): deploy the maintained fixture app to the
# local colima VM and verify it answers. Skips honestly (exit 0) when no
# local docker target exists.
quickstart:
	./examples/quickstart/run.sh

# R01 release receipts: build the goreleaser matrix locally, checksum,
# and diff against recorded expectations (see release/RELEASE_RECEIPT.md).
release-verify:
	./scripts/release-verify.sh verify

release-record:
	./scripts/release-verify.sh record

# R01 built-image smoke: build the container image from the verified
# matrix binary and run version + doctor in it. Skips without docker.
release-smoke:
	./scripts/release-verify.sh smoke
