package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
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
	ipAfter               time.Time
}

func (f *fake) Running(context.Context, string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
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
func (f *fake) IP(context.Context, string) (string, error) {
	if time.Now().Before(f.ipAfter) {
		return "", errors.New("booting")
	}
	return "127.0.0.1", nil
}
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

// testKeys lets a test add a key to a server from setup before it connects.
var testKeys = map[string]map[string]string{}

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
	vm := &VM{name: "dev", max: 2, hv: f, poll: time.Millisecond, tick: 20 * time.Millisecond, bootTimeout: 2 * time.Second,
		cfg:  VMConfig{Idle: duration(150 * time.Millisecond), Ports: []uint32{22}},
		dial: func(context.Context, string) (net.Conn, error) { return net.Dial("tcp", echo.Addr().String()) }}
	key = signer(t)
	keys := map[string]string{string(key.PublicKey().Marshal()): "dev"}
	s := newServer(map[string]*VM{"dev": vm}, keys, signer(t))
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	testKeys[l.Addr().String()] = keys
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
	if n, _ := f.counts(); n != 0 {
		t.Error("an unknown key started the VM")
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

func TestStartIsolates(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "tart")
	os.WriteFile(bin, []byte("#!/bin/sh\nfor a; do echo \"$a\"; done > "+out+"\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "softnet"), []byte("#!/bin/sh\n"), 0o755)
	tt := tart{bin, dir}

	t.Setenv("PATH", "/usr/bin:/bin")
	if err := tt.Start(t.Context(), "dev", VMConfig{}); err == nil {
		t.Fatal("started without softnet")
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	if err := tt.Start(t.Context(), "dev", VMConfig{}); err != nil {
		t.Fatal(err)
	}
	want := "run\ndev\n--no-graphics\n--net-softnet\n--net-softnet-block=out @host\n--net-softnet-allow=out proto udp @host port 53\n"
	var got []byte
	for range 100 {
		if got, _ = os.ReadFile(out); string(got) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%q", got)
}

// The messages have to show up in the ssh people really use.
func TestMessagesInOpenSSH(t *testing.T) {
	bin, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("no ssh")
	}
	f := &fake{ipAfter: time.Now().Add(150 * time.Millisecond)}
	addr, _ := setup(t, f)
	dir := t.TempDir()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := ssh.MarshalPrivateKey(priv, "")
	path := filepath.Join(dir, "k")
	os.WriteFile(path, pem.EncodeToMemory(block), 0o600)
	sg, _ := ssh.NewSignerFromKey(priv)
	testKeys[addr][string(sg.PublicKey().Marshal())] = "dev"
	host, port, _ := net.SplitHostPort(addr)
	run := func() string {
		cmd := exec.Command(bin, "-F", "none", "-i", path, "-p", port, "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes",
			"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-W", "dev:22", "x@"+host)
		cmd.Stdin = strings.NewReader("hi")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatal(err, string(out))
		}
		return string(out)
	}
	first := run()
	for _, want := range []string{"Connecting to your VM", "Creating your VM", "Starting your VM", "Still waiting", "Your VM is ready", "hi"} {
		if !strings.Contains(first, want) {
			t.Errorf("no %q in %s", want, first)
		}
	}
	if second := run(); !strings.Contains(second, "Connecting to your VM") || strings.Contains(second, "Starting") {
		t.Errorf("wrong messages when the VM was up: %s", second)
	}
	t.Log(first)
}
