package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Server is an SSH jump host and nothing else: no shell, no commands, no
// forwarding anywhere but the one VM a key belongs to.
type Server struct {
	vms       map[string]*VM
	ssh       *ssh.ServerConfig
	keepalive time.Duration
}

func newServer(vms map[string]*VM, keys map[string]string, hostKey ssh.Signer) *Server {
	c := &ssh.ServerConfig{
		MaxAuthTries: 6,
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			name, ok := keys[string(k.Marshal())]
			if !ok {
				return nil, errors.New("unknown key")
			}
			return &ssh.Permissions{Extensions: map[string]string{"vm": name}}, nil
		},
	}
	c.AddHostKey(hostKey)
	return &Server{vms: vms, ssh: c, keepalive: 30 * time.Second}
}

func (s *Server) serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go s.handle(c)
	}
}

func (s *Server) handle(raw net.Conn) {
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(20 * time.Second))
	conn, chans, reqs, err := ssh.NewServerConn(raw, s.login(raw))
	if err != nil {
		return
	}
	raw.SetDeadline(time.Time{})
	defer conn.Close()
	vm := s.vms[conn.Permissions.Extensions["vm"]]
	log.Printf("%s: connection from %s", vm.name, raw.RemoteAddr())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ssh.DiscardRequests(reqs)
	go s.watch(ctx, conn)
	for ch := range chans {
		if ch.ChannelType() != "direct-tcpip" {
			ch.Reject(ssh.Prohibited, "this only passes connections through to your VM")
			continue
		}
		var d struct {
			Host     string
			Port     uint32
			FromHost string
			FromPort uint32
		}
		if ssh.Unmarshal(ch.ExtraData(), &d) != nil || d.Host != vm.name || !vm.cfg.allows(d.Port) {
			ch.Reject(ssh.Prohibited, "not allowed")
			continue
		}
		if msg := conn.Permissions.Extensions["failed"]; msg != "" {
			ch.Reject(ssh.ConnectionFailed, msg)
			continue
		}
		go forward(ctx, vm, ch, d.Port)
	}
}

// login wakes the VM as the last step of signing in, because that's when SSH
// lets a server show messages. It runs only once a key's owner is proven.
func (s *Server) login(raw net.Conn) *ssh.ServerConfig {
	c := *s.ssh
	var pre ssh.ServerPreAuthConn
	c.PreAuthConnCallback = func(p ssh.ServerPreAuthConn) { pre = p }
	c.VerifiedPublicKeyCallback = func(_ ssh.ConnMetadata, _ ssh.PublicKey, perms *ssh.Permissions, _ string) (*ssh.Permissions, error) {
		vm := s.vms[perms.Extensions["vm"]]
		raw.SetDeadline(time.Now().Add(vm.bootTimeout + 20*time.Second))
		say := func(msg string) { pre.SendAuthBanner("[anytarts] " + msg + "\r\n") }
		if err := vm.wake(context.Background(), say); err != nil {
			log.Printf("%s: %v", vm.name, err)
			say("Sorry, " + explain(err) + ".")
			// Failing the login would only say "permission denied", so let it
			// through and refuse the forward instead.
			perms.Extensions["failed"] = explain(err)
		}
		return perms, nil
	}
	return &c
}

// explain is what a client gets to see of an error.
func explain(err error) string {
	if errors.Is(err, errNoSlot) {
		return err.Error()
	}
	return "the VM couldn't be reached"
}

// watch drops a client that has gone quiet, such as a closed laptop, which
// would otherwise hold its VM open.
func (s *Server) watch(ctx context.Context, conn ssh.Conn) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.keepalive):
		}
		answered := make(chan error, 1)
		go func() {
			_, _, err := conn.SendRequest("keepalive@openssh.com", true, nil)
			answered <- err
		}()
		select {
		case err := <-answered:
			if err != nil {
				conn.Close()
				return
			}
		case <-time.After(2 * s.keepalive):
			conn.Close()
			return
		case <-ctx.Done():
			return
		}
	}
}

func forward(ctx context.Context, vm *VM, nc ssh.NewChannel, port uint32) {
	vm.acquire()
	defer vm.release()
	up, err := vm.connect(ctx, port, func(string) {})
	if err != nil {
		log.Printf("%s: %v", vm.name, err)
		nc.Reject(ssh.ConnectionFailed, explain(err))
		return
	}
	defer up.Close()
	ch, reqs, err := nc.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	go ssh.DiscardRequests(reqs)
	stop := context.AfterFunc(ctx, func() { up.Close(); ch.Close() })
	defer stop()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(up, ch)
		if w, ok := up.(interface{ CloseWrite() error }); ok {
			w.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		io.Copy(ch, up)
		// The VM hung up, so nothing more can be sent to it either.
		ch.Close()
	}()
	wg.Wait()
}

func loadHostKey(dir string) (ssh.Signer, error) {
	path := filepath.Join(dir, "host_key")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		block, err := ssh.MarshalPrivateKey(priv, "anytarts")
		if err != nil {
			return nil, err
		}
		b = pem.EncodeToMemory(block)
		if err := os.WriteFile(path, b, 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(b)
}
