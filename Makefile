.PHONY: lint test build check dev dev-down conformance-up conformance conformance-down

lint:
	golangci-lint run ./...

test:
	go test ./... -race -count=1

build:
	go build ./...

# check 是合并前的门禁，必须只依赖本地能跑的东西。conformance 需要一个
# 真实的 kind+Cilium 集群，不属于这里——它有自己的三个 target，未设置
# DISTILL_CONFORMANCE_CONTEXT 时 `test` 里的那个子测试也会自行跳过。
check: lint test

dev:
	docker compose up --build

dev-down:
	docker compose down

# conformance-up/-down 起停 test/conformance/setup.sh 管理的 kind 集群；
# conformance 跑 harness 本身，默认连 setup.sh 建出来的 kind-distill。
conformance-up:
	test/conformance/setup.sh up

conformance:
	DISTILL_CONFORMANCE_CONTEXT=kind-distill go test ./test/conformance/ -v -count=1

conformance-down:
	test/conformance/setup.sh down
