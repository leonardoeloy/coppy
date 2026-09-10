.PHONY: build downloads test run
build:
	mkdir -p dist
	go build -o dist/coppy .
downloads:
	go run . --build-downloads
test:
	go test -race ./...
	go vet ./...
run:
	go run .
