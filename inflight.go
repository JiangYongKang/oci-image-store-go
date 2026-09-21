package ocistore

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

// call 表示一个进行中的同名调用。
type call struct {
	done chan struct{}
	err  error
}

// flightGroup 是极简 singleflight：同名并发调用只执行一次，其余共享结果。
type flightGroup struct {
	mu sync.Mutex
	m  map[string]*call
}

// Do 保证对同一 key 的 fn 在同一时刻只有一个执行；所有等待者得到同一结果。
func (g *flightGroup) Do(key string, fn func() error) error {
	g.mu.Lock()
	if g.m == nil {
		g.m = map[string]*call{}
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		<-c.done
		return c.err
	}
	c := &call{done: make(chan struct{})}
	g.m[key] = c
	g.mu.Unlock()

	c.err = fn()
	close(c.done)

	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()
	return c.err
}

func randomSuffix() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
