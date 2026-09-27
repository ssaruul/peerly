GO ?= go
DIST := dist
VERSION ?= 0.0.0

.PHONY: all test e2e server-arm64 server-amd64 windows installer icon cli clean

all: test server-arm64 server-amd64 windows cli

test:
	$(GO) vet ./...
	$(GO) test -race -count=1 ./...

e2e:
	rm -rf $(DIST)/e2e && mkdir -p $(DIST)/e2e/bin $(DIST)/e2e/shots
	$(GO) build -o $(DIST)/e2e/bin/server ./server
	$(GO) build -o $(DIST)/e2e/bin/app ./client/web
	PEERLY_E2E_DIR=$(abspath $(DIST)/e2e) python3 e2e/run.py

server-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "-s -w" -o $(DIST)/peerly-server-linux-arm64 ./server

server-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "-s -w" -o $(DIST)/peerly-server-linux-amd64 ./server

windows:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -trimpath -tags desktop,production -ldflags "-s -w -H windowsgui" -o $(DIST)/peerly.exe ./client/app
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags "-s -w" -o $(DIST)/peerly-browser.exe ./client/web
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags "-s -w" -o $(DIST)/peerly-cli.exe ./client/cli

# peerly-setup.exe: installs peerly.exe for the current user and adds it to the Start menu. Needs NSIS.
installer: windows
	makensis -V2 -DVERSION=$(VERSION) installer/peerly.nsi

# Redraws the icon and regenerates the resource file that embeds it into peerly.exe. Needs Pillow.
icon:
	python3 client/app/winres/make_icon.py
	$(GO) run github.com/tc-hib/go-winres@v0.3.3 make --in client/app/winres/winres.json --out client/app/rsrc --arch amd64

cli:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o $(DIST)/peerly-cli ./client/cli
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o $(DIST)/peerly-browser ./client/web

clean:
	rm -rf $(DIST)
