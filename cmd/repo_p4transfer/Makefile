BINARY=repo_p4transfer

# These are the values we want to pass for VERSION and BUILD
VERSION=`git describe --tags`
BUILD_DATE=`date +%FT%T%z`
USER=`git config user.email`
BRANCH=`git rev-parse --abbrev-ref HEAD`
REVISION=`git rev-parse --short HEAD`

# Setup the -ldflags option for go build here, interpolate the variable values.
MODULE="github.com/perforce/p4prometheus"
LOCAL_LDFLAGS=-ldflags="-X ${MODULE}/version.Version=${VERSION} -X ${MODULE}/version.BuildDate=${BUILD_DATE} -X ${MODULE}/version.Branch=${BRANCH} -X ${MODULE}/version.Revision=${REVISION} -X ${MODULE}/version.BuildUser=${USER}"
LDFLAGS=-ldflags="-w -s -X ${MODULE}/version.Version=${VERSION} -X ${MODULE}/version.BuildDate=${BUILD_DATE} -X ${MODULE}/version.Branch=${BRANCH} -X ${MODULE}/version.Revision=${REVISION} -X ${MODULE}/version.BuildUser=${USER}"

.PHONY: clean all test dist fmt vet

all: ${BINARY}

${BINARY}:
	go build -v ${LOCAL_LDFLAGS} -o ${BINARY}

# Builds distributions
dist:
	GOOS=darwin GOARCH=amd64 go build -o bin/${BINARY}-darwin-amd64 ${LDFLAGS}
	GOOS=darwin GOARCH=arm64 go build -o bin/${BINARY}-darwin-arm64 ${LDFLAGS}
	GOOS=linux GOARCH=amd64 go build -o bin/${BINARY}-linux-amd64 ${LDFLAGS}
	GOOS=linux GOARCH=arm64 go build -o bin/${BINARY}-linux-arm64 ${LDFLAGS}
	GOOS=windows GOARCH=amd64 go build -o bin/${BINARY}-windows-amd64.exe ${LDFLAGS}
	rm -f bin/${BINARY}*.gz
	-chmod +x bin/${BINARY}-*
	gzip bin/${BINARY}-*

clean:
	rm -f ${BINARY}
	rm -f *.out
	rm -rf bin/

fmt:
	go fmt ./...

vet:
	go vet ./...

test:
	go test -v ./...
