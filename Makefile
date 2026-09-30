BIN := bin/grimalkin

.PHONY: all build run test check clean

all: build

build:
	go build -o $(BIN) .

run: build
	./$(BIN)

check:
	scripts/check-imports.sh
	go vet ./...

test: check
	go test ./...

clean:
	rm -rf bin
