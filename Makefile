VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo 0.1.0)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOARCH ?= $(shell go env GOARCH)

.PHONY: build web binary run dev docker check hooks install uninstall clean

## build: web UI + Go binary -> bin/homestack
build: web binary

web:
	cd web && npm ci && npm run build

binary:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o bin/homestack .

## arm64: build for a Raspberry Pi 4/5 or other 64-bit ARM board
arm64:
	$(MAKE) binary GOARCH=arm64

## run: build and run locally (data in ./data)
run: build
	HOMESTACK_DATA=./data ./bin/homestack

## dev: run the Go server and the Vite dev server with hot reload
dev:
	@echo "Run 'HOMESTACK_DATA=./data go run .' in one terminal and 'cd web && npm run dev' in another."

## docker: build the container image (--pull: always on the latest base images)
docker:
	docker build --pull -t ghcr.io/leviathonbeast/homestack:latest --build-arg VERSION=$(VERSION) .

## check: everything that must pass before a push: lint, tests, known
##        vulnerabilities, latest releases (see scripts/check.sh)
check:
	bash scripts/check.sh

## hooks: run the checks automatically before every commit and push
hooks:
	git config core.hooksPath .githooks

## install: install the binary and systemd service (needs sudo)
install: build
	sudo install -m 0755 bin/homestack /usr/local/bin/homestack
	sudo install -m 0644 deploy/homestack.service /etc/systemd/system/homestack.service
	sudo mkdir -p /opt/stacks /var/lib/homestack
	sudo systemctl daemon-reload
	sudo systemctl enable --now homestack
	@echo "Homestack is running on http://$$(hostname -I | awk '{print $$1}'):5080"

uninstall:
	sudo systemctl disable --now homestack || true
	sudo rm -f /usr/local/bin/homestack /etc/systemd/system/homestack.service
	sudo systemctl daemon-reload
	@echo "Settings are kept in /var/lib/homestack and stacks in /opt/stacks."

clean:
	rm -rf bin web/dist/assets web/dist/index.html
