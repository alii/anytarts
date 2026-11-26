package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeSSH(t *testing.T, script string) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\n"+script), 0o755)
	t.Setenv("PATH", dir+":/usr/bin:/bin")
}

func TestSSHConfig(t *testing.T) {
	dir := t.TempDir()
	mine := "Host dev\n  User someone\nHost other\n  Port 99\n"
	os.WriteFile(filepath.Join(dir, "config"), []byte(mine), 0o600)
	for range 2 {
		if err := writeSSHConfig(dir, "box.example.net"); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := os.ReadFile(filepath.Join(dir, "config"))
	if string(got) != "Include ~/.ssh/anytarts.conf\n\n"+mine {
		t.Fatalf("%q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "config.before-anytarts")); string(b) != mine {
		t.Fatalf("backup %q", b)
	}
	out, err := exec.Command("/usr/bin/ssh", "-F", filepath.Join(dir, "anytarts.conf"), "-G", "dev").CombinedOutput()
	if err != nil {
		t.Fatal(string(out))
	}
	for _, want := range []string{"hostname dev\n", "user admin\n", "proxyjump box.example.net\n", "controlpersist 300\n"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("no %q", want)
		}
	}
}

func TestSetupRefusesOddHosts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, h := range []string{"-oProxyCommand=x", "a b", "a\nHost *", ""} {
		if setupClient([]string{h}) == nil {
			t.Errorf("took %q", h)
		}
	}
}

func TestRelay(t *testing.T) {
	fakeSSH(t, "exec cat\n")
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	done := make(chan struct{})
	go func() { relay(l, 200*time.Millisecond); close(done) }()
	for i := range 2 {
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(i, err)
		}
		fmt.Fprint(c, "hi")
		b := make([]byte, 2)
		if _, err := io.ReadFull(c, b); err != nil || string(b) != "hi" {
			t.Fatal(string(b), err)
		}
		c.Close()
		time.Sleep(50 * time.Millisecond)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("didn't stop after the last connection")
	}
}

func TestScreenshot(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.png")
	fakeSSH(t, `printf '\211PNG\r\n\032\nrest'`)
	if err := screenshot([]string{p}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); !strings.HasSuffix(string(b), "rest") {
		t.Fatalf("%q", b)
	}
	fakeSSH(t, "echo nope")
	if screenshot([]string{p + "2"}) == nil {
		t.Fatal("saved something that isn't a picture")
	}
	if _, err := os.Stat(p + "2"); err == nil {
		t.Fatal("left a file behind")
	}
}

func TestDiagnose(t *testing.T) {
	for in, want := range map[string]string{
		"admin@dev: Permission denied (publickey).": "key wasn't accepted",
		"both VM slots are in use, probably by CI":  "CI is using",
		"Host key verification failed.":             "haven't met",
	} {
		if got := diagnose(in); !strings.Contains(got, want) {
			t.Errorf("%q: %q", in, got)
		}
	}
}

func TestKeychainPathExpands(t *testing.T) {
	out, err := exec.Command("/bin/zsh", "-fc", "echo "+keychain).Output()
	if err != nil || !filepath.IsAbs(strings.TrimSpace(string(out))) {
		t.Fatalf("%q %v", out, err)
	}
}
