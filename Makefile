DEPS := $(shell go list -f '{{$$dir := .Dir}}{{range .GoFiles }}{{$$dir}}/{{.}} {{end}}' ./...)
BUILD = $(shell git rev-parse --short HEAD 2>/dev/null)
VERSION = $(shell git describe --tags)
LDFLAGS := "-X main.BUILD=$(BUILD) -X main.VERSION=$(VERSION)"

GitTag = $(shell git describe --abbrev=0 --tags 2>/dev/null || (echo '0.0.0'))
RepoTag := $(or $(CIRCLE_BUILD_NUM), ${GitTag})

build/linux/pentagon: $(DEPS)
	mkdir -p build/linux
	GOOS=linux go build $(GOMOD_RO_FLAG) -v -ldflags=$(LDFLAGS) -o ./build/linux/pentagon ./pentagon

build/darwin/pentagon: $(DEPS)
	mkdir -p build/darwin
	GOOS=darwin go build $(GOMOD_RO_FLAG) -v -ldflags=$(LDFLAGS) -o ./build/darwin/pentagon ./pentagon

.PHONY: docker
docker: Dockerfile $(DEPS)
	docker build . -t grafana/pentagon:${RepoTag}

.PHONY: docker-push
docker-push: Dockerfile $(DEPS)
	docker push grafana/pentagon:${RepoTag}

.PHONY: test
test:
	@go test -v ./...

.PHONY: clean
clean:
	@-rm -rf ./build ./vendor
