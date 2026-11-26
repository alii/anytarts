package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type fake struct {
	mu                    sync.Mutex
	exists, running       bool
	others, starts, stops int
}

func (f *fake) State(context.Context, string) (bool, bool, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.others
	if f.running {
		n++
	}
	return f.exists, f.running, n, nil
}
func (f *fake) Create(context.Context, string, VMConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exists = true
	return nil
}
func (f *fake) Start(context.Context, string, VMConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = true
	f.starts++
	return nil
}
func (f *fake) IP(context.Context, string) (string, error) { return "127.0.0.1", nil }
func (f *fake) Stop(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = false
	f.stops++
	return nil
}
func (f *fake) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.stops
}

func signer(t *testing.T) ssh.Signer {
	_, k, _ := ed25519.GenerateKey(rand.Reader)
	s, err := ssh.NewSignerFromKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func setup(t *testing.T, f *fake) (addr string, key ssh.Signer) {
	echo, _ := net.Listen("tcp", "127.0.0.1:0")
	t.Cleanup(func() { echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	vm := &VM{name: "dev", max: 2, hv: f, poll: time.Millisecond, bootTimeout: 2 * time.Second,
		cfg:  VMConfig{Idle: duration(150 * time.Millisecond), Ports: []uint32{22}},
		dial: func(context.Context, string) (net.Conn, error) { return net.Dial("tcp", echo.Addr().String()) }}
	key = signer(t)
	s := newServer(map[string]*VM{"dev": vm}, map[string]string{string(key.PublicKey().Marshal()): "dev"}, signer(t))
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	t.Cleanup(func() { l.Close() })
	go s.serve(l)
	return l.Addr().String(), key
}

func client(addr string, k ssh.Signer) (*ssh.Client, error) {
	return ssh.Dial("tcp", addr, &ssh.ClientConfig{User: "x", Auth: []ssh.AuthMethod{ssh.PublicKeys(k)}, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
}

func TestWakesAndIdles(t *testing.T) {
	f := &fake{}
	addr, k := setup(t, f)
	c, err := client(addr, k)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := f.counts(); n != 0 {
		t.Fatal("started before anything was forwarded")
	}
	p, err := c.Dial("tcp", "dev:22")
	if err != nil {
		t.Fatal(err)
	}
	p.Write([]byte("hi"))
	b := make([]byte, 2)
	if _, err := io.ReadFull(p, b); err != nil || string(b) != "hi" {
		t.Fatal(string(b), err)
	}
	time.Sleep(400 * time.Millisecond)
	if _, n := f.counts(); n != 0 {
		t.Fatal("stopped while in use")
	}
	c.Close()
	time.Sleep(50 * time.Millisecond)
	c2, _ := client(addr, k)
	if _, err := c2.Dial("tcp", "dev:22"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if _, n := f.counts(); n != 0 {
		t.Fatal("an old timer stopped it after a reconnect")
	}
	c2.Close()
	time.Sleep(400 * time.Millisecond)
	if a, b := f.counts(); a != 1 || b != 1 {
		t.Fatalf("starts %d stops %d", a, b)
	}
}

func TestRefuses(t *testing.T) {
	f := &fake{}
	addr, k := setup(t, f)
	if _, err := client(addr, signer(t)); err == nil {
		t.Fatal("let in an unknown key")
	}
	c, _ := client(addr, k)
	for _, to := range []string{"localhost:22", "127.0.0.1:22", "192.168.1.1:22", "other:22", "dev:80"} {
		if _, err := c.Dial("tcp", to); err == nil {
			t.Error("forwarded to", to)
		}
	}
	if _, err := c.NewSession(); err == nil {
		t.Error("gave a session")
	}
	if _, err := c.Listen("tcp", "127.0.0.1:0"); err == nil {
		t.Error("allowed a reverse forward")
	}
	if n, _ := f.counts(); n != 0 {
		t.Error("started the VM")
	}
}

func TestNoSlot(t *testing.T) {
	f := &fake{others: 2}
	addr, k := setup(t, f)
	c, _ := client(addr, k)
	if _, err := c.Dial("tcp", "dev:22"); err == nil || !strings.Contains(err.Error(), "slots are in use") {
		t.Fatal(err)
	}
}

func TestConfig(t *testing.T) {
	key := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer(t).PublicKey())))
	p := filepath.Join(t.TempDir(), "c.toml")
	os.WriteFile(p, []byte("state_dir='/x'\n[vm.dev]\nimage='ci-dev'\nkeys=['"+key+"']\nidle='5m'\n"), 0o600)
	c, keys, err := loadConfig(p)
	if err != nil || len(keys) != 1 || time.Duration(c.VMs["dev"].Idle) != 5*time.Minute || !c.VMs["dev"].isolated() || !c.VMs["dev"].allows(5900) {
		t.Fatal(c, err)
	}
	os.WriteFile(p, []byte("state_dir='/x'\n[vm.dev]\nimage='ci-dev'\nkeys=['"+key+"']\nisolat=false\n"), 0o600)
	if _, _, err := loadConfig(p); err == nil {
		t.Fatal("took a misspelt setting")
	}
}
