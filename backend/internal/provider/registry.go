package provider

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Registry lets the application select a provider without importing a
// concrete supplier from every business handler. Registration is explicit at
// startup, so adding an adapter cannot silently change the active route.
type Registry struct {
	mu    sync.RWMutex
	items map[string]Provider
}

func NewRegistry() *Registry { return &Registry{items: make(map[string]Provider)} }

func (r *Registry) Register(p Provider) error {
	if p == nil || strings.TrimSpace(p.Name()) == "" {
		return fmt.Errorf("provider name is required")
	}
	name := strings.ToLower(strings.TrimSpace(p.Name()))
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.items[name]; exists {
		return fmt.Errorf("provider %q is already registered", name)
	}
	r.items[name] = p
	return nil
}

func (r *Registry) Get(name string) (Provider, bool) {
	r.mu.RLock()
	p, ok := r.items[strings.ToLower(strings.TrimSpace(name))]
	r.mu.RUnlock()
	return p, ok
}

func (r *Registry) Names() []string {
	r.mu.RLock()
	names := make([]string, 0, len(r.items))
	for name := range r.items {
		names = append(names, name)
	}
	r.mu.RUnlock()
	sort.Strings(names)
	return names
}
