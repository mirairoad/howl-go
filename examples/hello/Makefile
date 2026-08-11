.PHONY: all generate run clean

MODULE := example.com/howl-hello

all: generate
	go build -o hello .

generate:
	go run github.com/mirairoad/howl-go/core/cmd/fsroutes -module $(MODULE)/client/pages
	go tool templ generate

run: all
	./hello

clean:
	rm -f hello howl-hello
