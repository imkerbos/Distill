package snapshot

// Service 按 (namespace, name) 查找 Service。
//
// 查找键必须含 namespace：同名 Service 在不同 namespace 是不同对象，
// 只按 name 查会把 kube-system 的 kube-dns 和别处的同名对象混为一谈。
func (a Assets) Service(namespace, name string) (Service, bool) {
	for _, s := range a.Services {
		if s.Namespace == namespace && s.Name == name {
			return s, true
		}
	}
	return Service{}, false
}

// EndpointsFor 按 (namespace, name) 查找 Endpoints。
func (a Assets) EndpointsFor(namespace, name string) (Endpoints, bool) {
	for _, e := range a.Endpoints {
		if e.Namespace == namespace && e.Name == name {
			return e, true
		}
	}
	return Endpoints{}, false
}
