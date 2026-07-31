package replay

import "time"

// Protocol is a transport-layer protocol.
type Protocol string

const (
	// ProtocolTCP is TCP.
	ProtocolTCP Protocol = "TCP"
	// ProtocolUDP is UDP.
	ProtocolUDP Protocol = "UDP"
	// ProtocolSCTP is SCTP.
	ProtocolSCTP Protocol = "SCTP"
)

// NamedPort is a named port declared by a Pod container.
type NamedPort struct {
	// Name is the port name.
	Name string
	// Port is the port number.
	Port int32
	// Protocol is the protocol.
	Protocol Protocol
}

// PodRef is a snapshot view of a Pod needed for evaluation.
//
// It contains only the fields actually used during evaluation: this package
// is pure functions, and importing the full corev1.Pod would drag in API
// version details and unrelated fields into the evaluation logic.
type PodRef struct {
	// ClusterID is the cluster the Pod belongs to. Policies can only select
	// Pods in the same cluster.
	ClusterID string
	// Namespace is the namespace the Pod is in.
	Namespace string
	// Name is the Pod name.
	Name string
	// IP is the Pod IP.
	IP string
	// Labels are the Pod's labels, which podSelector depends on.
	Labels map[string]string
	// HostNetwork indicates whether the Pod uses host networking, which is
	// not subject to NetworkPolicy control.
	HostNetwork bool
	// InMesh indicates whether a sidecar has been injected, making its L4
	// identity untrustworthy.
	InMesh bool
	// NamedPorts are the container ports declared by the Pod, used to resolve
	// named ports in policy rules.
	NamedPorts []NamedPort
}

// NamespaceRef is a snapshot view of a Namespace needed for evaluation.
type NamespaceRef struct {
	// ClusterID is the cluster the Namespace belongs to.
	ClusterID string
	// Name is the namespace name.
	Name string
	// Labels are the namespace's labels, which namespaceSelector depends on.
	Labels map[string]string
}

// Endpoint is one end of a flow.
type Endpoint struct {
	// ClusterID is the cluster this endpoint belongs to; empty for external
	// addresses.
	ClusterID string
	// IP is the endpoint's address.
	IP string
	// Pod is nil if identity could not be resolved (external address or
	// snapshot missing).
	Pod *PodRef
}

// Flow is a connection awaiting evaluation.
type Flow struct {
	// Source is the source endpoint.
	Source Endpoint
	// Dest is the destination endpoint.
	Dest Endpoint
	// Protocol is the transport-layer protocol.
	Protocol Protocol
	// Port is the destination port.
	Port int32
	// Timestamp is when the connection occurred, used to align with historical
	// snapshots.
	Timestamp time.Time
}
