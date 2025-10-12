# Set the Go command
GO := go

# Set the binary name
BINARY_NAME := simpleschema
BINARY_DIR := bin

# Default target
all: build

# Build the command
build: test
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BINARY_DIR)
	@$(GO) build -o $(BINARY_DIR)/$(BINARY_NAME) ./cmd/

# Run tests
test:
	@echo "Running tests..."
	@$(GO) test ./...

# Clean up
clean:
	@echo "Cleaning up..."
	@rm -rf $(BINARY_DIR)

.PHONY: all build test clean
