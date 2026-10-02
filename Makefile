BUILD_DIR := bin
BINARY_NAME := grimalkin
INSTALL_DIR := ~/go/bin

.PHONY: all build run test check install clean

all: build

check:
	scripts/check-imports.sh
	go vet ./...

test: check
	go test ./...

build:
	go build -o $(BUILD_DIR)/$(BINARY_NAME) .

run: build
	./$(BUILD_DIR)/$(BINARY_NAME)

install: check test build
	mkdir -p $(INSTALL_DIR)
	rm -rf $(INSTALL_DIR)/$(BINARY_NAME)
	mv $(BUILD_DIR)/$(BINARY_NAME) $(INSTALL_DIR)

clean:
	rm -rf bin

