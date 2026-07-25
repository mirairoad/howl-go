.PHONY: all core toy www clean

APPS := examples/toy_app www

all: core toy www

core:
	go build ./core/...

toy:
	$(MAKE) -C examples/toy_app

www:
	$(MAKE) -C www

# Run one or the other; they bind the same port by default.
run-toy: toy
	./examples/toy_app/toy_app

run-www: www
	./www/www

clean:
	$(MAKE) -C examples/toy_app clean
	$(MAKE) -C www clean
