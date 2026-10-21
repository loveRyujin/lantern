.PHONY: seed test eval build

export CGO_ENABLED := 0

seed:
	@echo "TODO: lantern-seed 尚未实现"

test:
	go test ./...

eval:
	@echo "TODO: lantern-eval 尚未实现"

build:
	go build ./...
