GO_SRC=$(shell find . -name \*.go)
COMMIT_HASH=$(shell git rev-parse HEAD)
COMMIT=$(if $(shell git status --porcelain --untracked-files=no),$(COMMIT_HASH)-dirty,$(COMMIT_HASH))

default: vendor $(GO_SRC)
	go build -ldflags "-X main.version=$(COMMIT)" -o octoci github.com/anuvu/octoci

vendor: glide.lock
	glide install --strip-vendor

.PHONY: vendorup
vendorup:
	glide cc
	glide up --strip-vendor

clean:
	-rm -r vendor
