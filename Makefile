REPO_ROOT:=${CURDIR}
OUT_DIR=$(REPO_ROOT)/bin
# disable CGO by default for static binaries
CGO_ENABLED=0
export GOROOT GO111MODULE CGO_ENABLED

build: build-driver build-controller

build-driver:
	go build -v -o "$(OUT_DIR)/driver" ./cmd/driver

build-controller:
	go build -v -o "$(OUT_DIR)/controller" ./cmd/controller

clean:
	rm -rf "$(OUT_DIR)/"

test:
	CGO_ENABLED=1 go test -v -race -count 1 ./...

# code linters
lint:
	hack/lint.sh

update:
	go mod tidy

# Container image configuration
REGISTRY?=aojea
# tag based on date-sha
TAG?=$(shell echo "$$(date +v%Y%m%d)-$$(git describe --always --dirty)")

# Driver image configuration
DRIVER_IMAGE_NAME=dravip
DRIVER_IMAGE?=$(REGISTRY)/$(DRIVER_IMAGE_NAME)
DRIVER_TAGGED_IMAGE?=$(REGISTRY)/$(DRIVER_IMAGE_NAME):$(TAG)

# Controller image configuration  
CONTROLLER_IMAGE_NAME=dravip-controller
CONTROLLER_IMAGE?=$(REGISTRY)/$(CONTROLLER_IMAGE_NAME)
CONTROLLER_TAGGED_IMAGE?=$(REGISTRY)/$(CONTROLLER_IMAGE_NAME):$(TAG)

# required to enable buildx
export DOCKER_CLI_EXPERIMENTAL=enabled

# Build driver container image
image-driver:
	docker build --network host . -t ${DRIVER_TAGGED_IMAGE} --target driver --load

# Build controller container image
image-controller:
	docker build --network host . -t ${CONTROLLER_TAGGED_IMAGE} --target controller --load

# Build both images
images: image-driver image-controller
