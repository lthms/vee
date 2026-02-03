PREFIX  ?= $(HOME)/.local
BINDIR  ?= $(PREFIX)/bin
DATADIR ?= $(PREFIX)/share/vee

.PHONY: build install

build:
	go build -o vee ./cmd/vee

install: build
	install -d $(BINDIR)
	install -m 755 vee $(BINDIR)/vee
	rm -rf $(DATADIR)/plugins/vee
	install -d $(DATADIR)/plugins
	cp -r plugins/vee $(DATADIR)/plugins/vee
	install -d $(DATADIR)/modes
	cp modes/*.md $(DATADIR)/modes/
