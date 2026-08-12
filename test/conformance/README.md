# Conformance harness

This package cross-validates `internal/replay` — Distill's NetworkPolicy
evaluation engine — against a real Kubernetes cluster running Cilium. Same
policies, same traffic, two independent implementations. Every disagreement
is either a defect in the engine or a misunderstanding of NetworkPolicy
semantics on our part.

This exists because `internal/replay` is the platform's evaluation layer:
CLAUDE.md requires it to be close to 100% correct, and states that human
review cannot guarantee that. Golden tests alone only cover the scenarios
their author thought of. This harness asks a cluster that has no idea how
we implemented the engine to grade our answers.

## What it proves

For each of the nine connection shapes in `manifests/`:

| case | tests |
|---|---|
| `namespace-selector-allow` | `namespaceSelector` positive match |
| `namespace-selector-deny` | `namespaceSelector` negative match (implicit deny via a default-deny-ingress + one narrow allow) |
| `unlabelled-pod-deny` | a Pod with no `app` label still evaluates correctly by its actual labels |
| `named-port` | policy `port: http` resolves against the Pod's declared named port, not a literal string match |
| `port-range-in-range` | `endPort` range, destination port inside the range |
| `port-range-out-of-range` | `endPort` range, destination port outside the range |
| `ipblock-except` | `ipBlock.cidr` + `ipBlock.except` |
| `egress-allow` | egress-side evaluation order, allowed peer |
| `egress-deny-no-ingress-policy` | egress-side deny where the destination has no ingress policy at all — the denial can only have come from the egress side |

Every case is checked two ways: the engine's verdict must match the
declarative table in `conformance_test.go`, and Cilium's real, observed
verdict (via Hubble) must also match the table. A case that appears in the
table but produces zero observed flows fails loudly instead of silently
passing — see the "two lessons" section below for why that distinction
matters.

## What it does not prove

A green run here is evidence, not a certificate. It does not exercise:

- **hostNetwork subjects.** `manifests/workloads.yaml` deploys a
  hostNetwork Pod (`kube-system/node-agent`) so the snapshot layer sees
  one, but no traffic is routed to or from it. `IsUnmanaged` and the
  "NetworkPolicy doesn't apply here" path are covered by golden tests, not
  by this harness.
- **Cross-cluster peers.** This is a single kind cluster; `CrossCluster`
  and the ipBlock-only fallback for cross-cluster traffic are untested
  here.
- **UDP.** All matrix traffic is TCP (`wget`/`nc -z`). DNS (UDP/53) flows
  exist in the cluster but aren't asserted on. Port matching for UDP goes
  through the same `portMatches` code path but is not independently
  verified against Cilium by this harness.
- **Mesh / CCNP degradation.** No service mesh sidecar and no
  CiliumClusterwideNetworkPolicy is installed, so `ConfidenceDegraded` is
  never exercised here. That path has its own golden tests
  (`internal/replay/degraded_test.go`).
- **Unresolvable named ports.** `named-port` only tests the case where the
  name resolves. A policy naming a port the Pod never declares is not in
  this matrix.
- **External-CIDR egress.** All ipBlock traffic in this matrix stays
  inside the Pod CIDR. Egress to a genuinely external address (the world,
  a public API) is not covered.

Do not read a green run as "the engine is fully verified." Read it as "the
engine agrees with a real enforcement plane on these nine shapes."

## Two lessons this harness had to learn the hard way

Both mistakes made an earlier throwaway version of this harness *report
better results than reality* — the dangerous direction, since a harness
that overstates correctness is worse than no harness.

1. **Collect both traffic directions.** A connection denied by an egress
   policy never leaves the source node, so the destination side never
   emits a flow. An early version filtered to `INGRESS` only and silently
   dropped the entire egress half of the matrix while still appearing to
   pass — there was simply no flow to disagree with. `collectFlows` in
   `conformance_test.go` does not filter by `traffic_direction` for
   exactly this reason.

2. **The effective verdict is "any direction dropped wins", not a
   majority vote.** A connection is broken if either direction denies it.
   Majority would let a connection that was cut by an egress policy — after
   emitting many forwarded heartbeats before the policy took effect, or on
   an unrelated path — read as working. See `effectiveVerdict` in
   `conformance_test.go`.

## Running it

The cluster is not part of `make check` or CI. Bring it up, run the
harness, tear it down:

```sh
make conformance-up      # creates the kind cluster, installs Cilium, applies manifests
make conformance         # DISTILL_CONFORMANCE_CONTEXT=kind-distill go test ./test/conformance/ -v -count=1
make conformance-down    # tears the cluster down
```

Or drive `setup.sh` and `go test` directly:

```sh
test/conformance/setup.sh up
DISTILL_CONFORMANCE_CONTEXT=kind-distill go test ./test/conformance/ -v -count=1
test/conformance/setup.sh down
```

Environment variables:

- `DISTILL_CONFORMANCE_CONTEXT` (required) — the kube context to read the
  cluster from. Unset means skip; there is no "current context" fallback,
  on purpose.
- `DISTILL_CONFORMANCE_HUBBLE` (optional) — the Hubble relay address.
  Defaults to `localhost:4245`, which is what `setup.sh up` forwards.

## Why shell out to the `hubble` CLI

The harness runs `hubble observe -o json` as a subprocess instead of
importing `github.com/cilium/cilium` and talking to Hubble's gRPC API
directly. That module is large and would drag transitive `k8s.io`
dependencies — and potentially the `go` directive — upward, which conflicts
with the pin in `go.mod` (`go 1.25.0`, `k8s.io/api` /
`k8s.io/apimachinery` / `k8s.io/client-go` all at `v0.35.0`, see
CLAUDE.md §8). The CLI's JSON output is a stable, public interface and is
sufficient for what this harness needs.
