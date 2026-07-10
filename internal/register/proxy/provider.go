package proxy

import (
	"strings"
	"sync"
)

type Provider interface {
	Next() string
	All() []string
}

type Static struct {
	URL string
}

func (s Static) Next() string { return strings.TrimSpace(s.URL) }
func (s Static) All() []string {
	if url := strings.TrimSpace(s.URL); url != "" {
		return []string{url}
	}
	return nil
}

type Pool struct {
	mu    sync.Mutex
	urls  []string
	index int
}

func NewPool(urls []string) *Pool {
	clean := make([]string, 0, len(urls))
	for _, url := range urls {
		if url = strings.TrimSpace(url); url != "" {
			clean = append(clean, url)
		}
	}
	return &Pool{urls: clean}
}

func (p *Pool) Next() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.urls) == 0 {
		return ""
	}
	url := p.urls[p.index%len(p.urls)]
	p.index++
	return url
}

func (p *Pool) All() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.urls))
	copy(out, p.urls)
	return out
}

// New builds a provider from primary proxy + optional pool.
// Empty primary with empty pool returns a direct (no-proxy) Static.
func New(primary string, pool []string) Provider {
	primary = strings.TrimSpace(primary)
	cleanPool := make([]string, 0, len(pool)+1)
	if primary != "" {
		cleanPool = append(cleanPool, primary)
	}
	for _, url := range pool {
		if url = strings.TrimSpace(url); url != "" && url != primary {
			cleanPool = append(cleanPool, url)
		}
	}
	if len(cleanPool) == 0 {
		return Static{}
	}
	if len(cleanPool) == 1 {
		return Static{URL: cleanPool[0]}
	}
	return NewPool(cleanPool)
}
