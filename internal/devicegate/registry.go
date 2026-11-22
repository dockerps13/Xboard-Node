// Package devicegate implements atomic distinct-IP admission for one
// authorization authority. This in-memory registry is a building block for
// per-machine tests; it is NOT distributed admission across machines.
package devicegate

import (
	"net/netip"
	"sync"
)

// Registry tracks admitted connections grouped by panel scope and user ID.
// Every acquisition has its own release token. IPs share one device slot.
type Registry struct {
	mu     sync.Mutex
	owners map[key]map[string]int
}

type key struct {
	scope  string
	userID int
}

// New constructs an empty device admission authority.
func New() *Registry {
	return &Registry{owners: make(map[key]map[string]int)}
}

// Acquire atomically admits one client IP if its account has capacity.
// Limit <= 0 disables the cap. A successful call returns a release callback
// that is safe to call repeatedly. Rejecting an invalid identity fails closed.
func (r *Registry) Acquire(scope string, userID int, rawIP string, limit int) (release func(), ok bool) {
	if r == nil || scope == "" || userID <= 0 {
		return nil, false
	}
	addr, err := netip.ParseAddr(rawIP)
	if err != nil {
		return nil, false
	}
	ip := addr.WithZone("").Unmap().String()
	k := key{scope: scope, userID: userID}

	r.mu.Lock()
	ips := r.owners[k]
	if ips == nil {
		ips = make(map[string]int)
		r.owners[k] = ips
	}
	if limit > 0 && ips[ip] == 0 && len(ips) >= limit {
		// Do not leave an empty scope behind on rejection.
		if len(ips) == 0 {
			delete(r.owners, k)
		}
		r.mu.Unlock()
		return nil, false
	}
	ips[ip]++
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			ips := r.owners[k]
			if ips[ip] <= 1 {
				delete(ips, ip)
			} else {
				ips[ip]--
			}
			if len(ips) == 0 {
				delete(r.owners, k)
			}
		})
	}, true
}

// DistinctIPs reports the number of admitted distinct IPs for a scope/user.
func (r *Registry) DistinctIPs(scope string, userID int) int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.owners[key{scope: scope, userID: userID}])
}
