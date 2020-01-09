#IMG ?= openshift/origin-cluster-csi-snapshot-controller-operator:latest

PACKAGE=github.com/openshift/cluster-csi-snapshot-controller-operator
MAIN_PACKAGE=$(PACKAGE)/cmd/csi-snapshot-controller-manager

BIN=$(lastword $(subst /, ,$(MAIN_PACKAGE)))

GOOS=linux
GO_BUILD_RECIPE=GOOS=$(GOOS) CGO_ENABLED=0 go build -o $(BIN) $(MAIN_PACKAGE)

BINDATA=pkg/generated/bindata.go
FIRST_GOPATH := $(firstword $(subst :, ,$(shell go env GOPATH)))
GOBINDATA_BIN=$(FIRST_GOPATH)/bin/go-bindata

all: build

build: generate
	$(GO_BUILD_RECIPE)

generate: $(GOBINDATA_BIN)
	$(GOBINDATA_BIN) -nometadata -pkg generated -o $(BINDATA) assets/...

$(GOBINDATA_BIN):
	go build -o $(GOBINDATA_BIN) ./vendor/github.com/jteeuwen/go-bindata/go-bindata

test:
	go test ./pkg/...

container: build test verify
	docker build . -t $(IMG)

clean:
	go clean
	rm -f $(BIN)
