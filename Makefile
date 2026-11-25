.PHONY: build test test-race smoke smoke-all fmt vet

build:
	go build -o mini-container .

test:
	go test ..

test-race:
	go test -race ..

vet:
	go vet ..

fmt:
	gofmt -w main.go main_test.go

# Smoke tests require a Linux VM with root privileges and Alpine rootfs.
smoke: build
	./tests/smoke.sh

# Includes systemd cgroup tests. Run with: sudo make smoke-all
smoke-all: build
	RUN_CGROUP_TEST=1 ./tests/smoke.sh
