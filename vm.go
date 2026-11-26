package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

var errNoSlot = errors.New("both VM slots are in use, probably by CI; try again soon")

type VM struct {
	name string
	cfg  VMConfig
	max  int
	hv   Hypervisor
	dial func(ctx context.Context, addr string) (net.Conn, error)

	poll, bootTimeout time.Duration

	// power is held while starting or stopping, so the two never overlap.
	power sync.Mutex

	mu    sync.Mutex
	users int
	gen   int // bumped on every connect and disconnect; stale timers see it moved
}

func (v *VM) acquire() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.users++
	v.gen++
}

func (v *VM) release() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.users--
	v.gen++
	if v.users == 0 {
		gen := v.gen
		time.AfterFunc(time.Duration(v.cfg.Idle), func() { v.idle(gen) })
	}
}

func (v *VM) idle(gen int) {
	v.power.Lock()
	defer v.power.Unlock()
	v.mu.Lock()
	stale := v.users > 0 || v.gen != gen
	v.mu.Unlock()
	if stale {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	_, running, _, err := v.hv.State(ctx, v.name)
	if err == nil && !running {
		return
	}
	if err == nil {
		log.Printf("%s: idle, shutting down", v.name)
		err = v.hv.Stop(ctx, v.name)
	}
	if err != nil {
		log.Printf("%s: shutting down: %v; trying again later", v.name, err)
		time.AfterFunc(time.Duration(v.cfg.Idle), func() { v.idle(gen) })
	}
}

// connect starts the VM if it's off and returns a connection to port, waiting
// for it to answer. The caller holds an acquire, so idle can't stop it.
func (v *VM) connect(ctx context.Context, port uint32) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, v.bootTimeout)
	defer cancel()
	if err := v.ensure(ctx); err != nil {
		return nil, err
	}
	var last error
	for {
		ip, err := v.hv.IP(ctx, v.name)
		if err == nil {
			var c net.Conn
			if c, err = v.dial(ctx, net.JoinHostPort(ip, fmt.Sprint(port))); err == nil {
				return c, nil
			}
		}
		last = err
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s didn't answer: %w", v.name, last)
		case <-time.After(v.poll):
		}
	}
}

func (v *VM) ensure(ctx context.Context) error {
	v.power.Lock()
	defer v.power.Unlock()
	exists, running, total, err := v.hv.State(ctx, v.name)
	if err != nil || running {
		return err
	}
	if total >= v.max {
		return errNoSlot
	}
	if !exists {
		log.Printf("%s: creating from %s", v.name, v.cfg.Image)
		if err := v.hv.Create(ctx, v.name, v.cfg); err != nil {
			return err
		}
	}
	log.Printf("%s: starting", v.name)
	if err := v.hv.Start(ctx, v.name, v.cfg); err != nil {
		return err
	}
	for range 10 {
		time.Sleep(v.poll)
		if _, running, _, err := v.hv.State(ctx, v.name); err == nil && running {
			return nil
		}
	}
	return fmt.Errorf("%s didn't start; its log is in the state folder", v.name)
}
