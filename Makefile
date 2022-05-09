GO_SRC=$(shell find . -name \*.go)

default: $(GO_SRC)
	go build

.PHONY: check
check:
	go fmt ./... && ([ -z $(TRAVIS) ] || git diff --quiet)
	cd test/ && sudo ./main.sh
