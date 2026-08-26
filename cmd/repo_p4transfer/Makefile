.PHONY: clean all

all: repo_p4transfer

repo_p4transfer:
	go build -v -o repo_p4transfer

clean:
	rm -f repo_p4transfer
	rm -f *.out

fmt:
	go fmt ./...

vet:
	go vet ./...

test:
	go test -v ./...
