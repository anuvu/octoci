GO_SRC=$(shell find . -name \*.go)
MAIN_VERSION=1.0.0

default: $(GO_SRC)
	go build -ldflags "-X main.version=$(MAIN_VERSION)"

.PHONY: check
check:
	go fmt ./... && ([ -z $(TRAVIS) ] || git diff --quiet)
	cd test/ && sudo ./main.sh
