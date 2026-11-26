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
	for _, want := range []string{"hostname dev\n", "user admin\n", "controlpersist 300\n"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("no %q", want)
		}
	}
}

func TestProxyHost(t *testing.T) {
	dir := t.TempDir()
	writeSSHConfig(dir, "box.example.net")
	out, _ := exec.Command("/usr/bin/ssh", "-F", filepath.Join(dir, "anytarts.conf"), "-G", "dev").Output()
	for line := range strings.Lines(string(out)) {
		if m := proxyHost.FindStringSubmatch(strings.TrimSpace(line)); m != nil && m[1] == "box.example.net" {
			return
		}
	}
	t.Fatalf("doctor can't find the host in %s", out)
}

func TestForwardArgs(t *testing.T) {
	got, err := forwardArgs([]string{"8080", "3000:80"})
	if err != nil || !strings.Contains(strings.Join(got, " "), "-L 127.0.0.1:8080:localhost:8080 -L 127.0.0.1:3000:localhost:80 dev") {
		t.Fatal(got, err)
	}
	for _, bad := range []string{"", "0", "70000", "a:1", "1:2:3", "-D1", "1:"} {
		if _, err := forwardArgs([]string{bad}); err == nil {
			t.Errorf("took %q", bad)
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
	fakeSSH(t, `[ "$1" = -O ] && exit 0; printf '\211PNG\r\n\032\nrest'`)
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

func TestScreenshotTwoDisplays(t *testing.T) {
	dir := t.TempDir()
	// The message simctl really gave for a foldable.
	fakeSSH(t, `[ "$1" = -O ] && exit 0; case "$2" in
*--display=*) printf '\211PNG\r\n\032\n'; echo "$2" ;;
*) cat >&2 <<X
An error was encountered processing the command (domain=NSPOSIXErrorDomain, code=22):
Cannot choose between 2 displays. Specify one with --display:
    --display=3  LCD-1 (primary-1) 2007x2853
    --display=1  LCD (primary) 1398x2034
Invalid argument
X
exit 22 ;;
esac
`)
	if err := screenshot([]string{filepath.Join(dir, "a.png")}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"1", "3"} {
		b, _ := os.ReadFile(filepath.Join(dir, "a-display"+n+".png"))
		if !strings.Contains(string(b), "--display="+n+" ") {
			t.Errorf("display %s: %q", n, b)
		}
	}
	if err := screenshot([]string{"--display=3", filepath.Join(dir, "b.png")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.png")); err != nil {
		t.Fatal(err)
	}
	if screenshot([]string{"--display=1;id"}) == nil {
		t.Fatal("took an odd display")
	}
	f, _ := os.ReadDir(dir)
	if len(f) != 3 {
		t.Fatal(f)
	}
}
