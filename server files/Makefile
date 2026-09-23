.PHONY: build run deps

deps:
	go mod tidy

build: deps
	go build -o game_server .

run: build
	./game_server
