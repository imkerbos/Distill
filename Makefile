.PHONY: lint test build check dev dev-down test-integration conformance-up conformance conformance-down

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

# MySQL 集成测试跑在 distill_test 上，不是 distill。测试会 truncate 业务表，
# 打到 dev 库会清掉种子集群与全部覆盖决定——已经发生过一次。库名写死在这里，
# 就不必每次手敲 DSN，也就不会敲错。
#
# -p 1 是必需的：两个包清空的是同一个库里的同一批表，并行跑会互相删掉
# 对方正在用的行，表现为随机失败的测试。
test-integration:
	docker compose exec -T mysql mysql -uroot -pdistill-local \
	  -e "CREATE DATABASE IF NOT EXISTS distill_test CHARACTER SET utf8mb4;"
	docker compose exec -T \
	  -e DISTILL_TEST_MYSQL_DSN='root:distill-local@tcp(mysql:3306)/distill_test?parseTime=true&loc=UTC' \
	  distill-api go test ./internal/mysqlregistry/ ./internal/snapshotstore/ ./internal/collectstore/ \
	  -count=1 -p 1

# conformance-up/-down 起停 test/conformance/setup.sh 管理的 kind 集群；
# conformance 跑 harness 本身，默认连 setup.sh 建出来的 kind-distill。
conformance-up:
	test/conformance/setup.sh up

conformance:
	DISTILL_CONFORMANCE_CONTEXT=kind-distill go test ./test/conformance/ -v -count=1

conformance-down:
	test/conformance/setup.sh down
