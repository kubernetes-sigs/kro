.PHONY: build run clean test

build:
	go build -o performance_cli simple_cli.go

run: build
	./performance_cli

benchmark: build
	./performance_cli benchmark

analyze: build
	./performance_cli analyze

visualize: build
	./performance_cli visualize

clean:
	rm -f performance_cli

test:
	go test -v ./...
