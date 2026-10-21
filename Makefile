.PHONY: seed test eval build

export CGO_ENABLED := 0

seed:
	go run ./cmd/lantern-seed -force

test:
	go test ./...

eval:
	@echo "TODO: lantern-eval 尚未实现"

build:
	go build ./...
