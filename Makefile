.PHONY: lint test build check web-check dev dev-down test-integration conformance-up conformance conformance-down

lint:
	golangci-lint run ./...

test:
	go test ./... -race -count=1

build:
	go build ./...

# check 是合并前的门禁，必须只依赖本地能跑的东西。conformance 需要一个
# 真实的 kind+Cilium 集群，不属于这里——它有自己的三个 target，未设置
# DISTILL_CONFORMANCE_CONTEXT 时 `test` 里的那个子测试也会自行跳过。
# purity 与 lint / test 并列进 check：它守的是一条编译产物的性质
# （装进客户集群的那个二进制不得链接状态库），而那种性质靠 review 守不住。
check: lint test purity web-check

purity:
	./scripts/check-push-purity.sh

# 前端进门禁。此前它一次都没被自动跑过：CI 只有 golangci-lint 与 go test，
# check 只有 lint/test/purity —— 于是前端的类型、lint 与那 30 多个测试文件
# 全靠人记得手动跑。一个跑不起来的前端与一个跑得起来的前端在门禁上长得
# 一模一样。
#
# **必须走 `npm run typecheck`（即 `tsc -b`），不能写成 `npx tsc --noEmit`。**
# web/tsconfig.json 是 solution 式的（"files": []，只有 references），
# --noEmit 对着它检查零个文件、静默返回 0 —— 一道恒绿的门禁比没有门禁更糟。
web-check:
	cd web && npm run check

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

# 镜像构建。VERSION 进 ldflags —— 平台会把策略下发到生产集群，事故回溯时
# 必须能确定当时跑的是哪个版本，而 "dev" 回答不了这个问题。
IMAGE_REGISTRY ?= REPLACE_ME
VERSION ?= $(shell git rev-parse --short HEAD)

.PHONY: image-api image-collector image-agent images

image-api:
	docker build -f build/Dockerfile.api --build-arg VERSION=$(VERSION) \
	  -t $(IMAGE_REGISTRY)/distill-api:$(VERSION) .

image-collector:
	docker build -f build/Dockerfile.collector --build-arg VERSION=$(VERSION) \
	  -t $(IMAGE_REGISTRY)/distill-collector:$(VERSION) .

# agent 装进别人的集群，与上面两个不是同一类东西：它的镜像是 scratch，
# 且有一条门禁守着它不得链接平台状态库（make purity）。
image-agent: purity
	docker build -f build/Dockerfile.agent --build-arg VERSION=$(VERSION) \
	  -t $(IMAGE_REGISTRY)/distill-agent:$(VERSION) .

images: image-api image-collector image-agent
