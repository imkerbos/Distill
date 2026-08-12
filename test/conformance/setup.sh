#!/usr/bin/env bash
# 起停 conformance harness 用的 kind + Cilium 集群。
#
# 集群本身不是常驻基础设施——它只在需要重新验证求值引擎时才存在，
# 用完随手 down 掉。up/down 都是幂等的：重复 up 会跳过已存在的集群，
# down 找不到集群也不报错。
set -euo pipefail

CLUSTER_NAME="distill"
KUBE_CONTEXT="kind-${CLUSTER_NAME}"
CILIUM_VERSION="1.19.5"
HUBBLE_LOCAL_PORT="4245"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFEST_DIR="${SCRIPT_DIR}/manifests"
PF_PID_FILE="/tmp/distill-conformance-hubble-pf.pid"
PF_LOG_FILE="/tmp/distill-conformance-hubble-pf.log"

usage() {
	cat <<EOF
Usage: $(basename "$0") up|down

  up    创建 kind 集群、装 Cilium + Hubble relay、下发 manifests/、
        起 hubble-relay 的端口转发。
  down  停掉端口转发、删掉 kind 集群。
EOF
}

up() {
	local kind_config
	kind_config="$(mktemp -t distill-conformance-kind-XXXX.yaml)"
	trap 'rm -f "${kind_config}"' RETURN

	cat >"${kind_config}" <<'KINDCFG'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: distill
networking:
  # Cilium 自己装 CNI；留着 kindnet 会两个 CNI 打架。
  disableDefaultCNI: true
  # 保留 kube-proxy：Cilium 的 kubeProxyReplacement 在 Docker Desktop
  # 的虚拟机内核上常缺 BPF 特性，不值得为它在这一步引入变量。
  kubeProxyMode: "iptables"
nodes:
  - role: control-plane
  - role: worker
KINDCFG

	if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
		echo "kind cluster '${CLUSTER_NAME}' already exists, reusing it"
	else
		kind create cluster --config "${kind_config}"
	fi

	echo "installing Cilium ${CILIUM_VERSION} with Hubble + relay enabled..."
	cilium install --version "${CILIUM_VERSION}" --context "${KUBE_CONTEXT}" \
		--set kubeProxyReplacement=false \
		--set ipam.mode=kubernetes \
		--set routingMode=tunnel \
		--set tunnelProtocol=vxlan \
		--set operator.replicas=1 \
		--set hubble.enabled=true \
		--set hubble.relay.enabled=true

	cilium status --context "${KUBE_CONTEXT}" --wait

	echo "applying conformance manifests from ${MANIFEST_DIR}..."
	kubectl --context "${KUBE_CONTEXT}" apply -f "${MANIFEST_DIR}"

	echo "waiting for hubble-relay to be ready..."
	kubectl --context "${KUBE_CONTEXT}" -n kube-system rollout status deploy/hubble-relay --timeout=180s

	stop_port_forward

	echo "starting hubble-relay port-forward on localhost:${HUBBLE_LOCAL_PORT} (background)..."
	nohup kubectl --context "${KUBE_CONTEXT}" -n kube-system \
		port-forward svc/hubble-relay "${HUBBLE_LOCAL_PORT}:80" \
		>"${PF_LOG_FILE}" 2>&1 &
	echo $! >"${PF_PID_FILE}"
	sleep 2

	cat <<EOF

环境已就绪。接下来跑：

  DISTILL_CONFORMANCE_CONTEXT=${KUBE_CONTEXT} go test ./test/conformance/ -v -count=1

或者：

  make conformance

用完之后：

  $(basename "$0") down
EOF
}

stop_port_forward() {
	if [ -f "${PF_PID_FILE}" ]; then
		kill "$(cat "${PF_PID_FILE}")" 2>/dev/null || true
		rm -f "${PF_PID_FILE}"
	fi
}

down() {
	stop_port_forward
	if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
		kind delete cluster --name "${CLUSTER_NAME}"
	else
		echo "kind cluster '${CLUSTER_NAME}' does not exist, nothing to delete"
	fi
}

case "${1:-}" in
up)
	up
	;;
down)
	down
	;;
*)
	usage
	exit 1
	;;
esac
