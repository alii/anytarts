package main

import (
	"bytes"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed guide.md
var guide string

const usage = `anytarts controls your VM from your own Mac.

  anytarts setup HOST       set this Mac up to use the VM behind HOST
  anytarts ssh [COMMAND]    open a terminal in the VM, or run a command there
  anytarts screen           see the VM's screen
  anytarts screenshot [F]   save a picture of the VM's iPhone simulator
  anytarts unlock           unlock the VM's keychain
  anytarts doctor           find out what's wrong
  anytarts guide            print the guide for agents (--install adds it to Claude Code)

On the Mac that runs the VMs:

  anytarts -config FILE
`

// The VM's shell expands the ~. security doesn't look for relative paths in the
// current directory, so it needs the whole path.
const keychain = "~/Library/Keychains/login.keychain-db"

// vmName is what the VM is called, both in the doorman's config and to ssh.
var vmName = "dev"

func client(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}
	if v := os.Getenv("ANYTARTS_VM"); v != "" {
		vmName = v
	}
	if !validName.MatchString(vmName) {
		return fmt.Errorf("%q isn't a VM name", vmName)
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "setup":
		return setupClient(args)
	case "ssh":
		return become("ssh", append([]string{vmName}, args...)...)
	case "unlock":
		return become("ssh", "-t", vmName, "security unlock-keychain "+keychain)
	case "screen":
		return screen()
	case "screenshot":
		return screenshot(args)
	case "doctor":
		return doctor()
	case "guide":
		return printGuide(args)
	case "help":
		fmt.Print(usage)
		return nil
	}
	fmt.Fprint(os.Stderr, usage)
	return fmt.Errorf("there's no %q command", cmd)
}

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)

// become replaces anytarts with another program, so it gets the terminal and
// its exit status is ours.
func become(name string, args ...string) error {
	path, err := exec.LookPath(name)
	if err != nil {
		return err
	}
	return syscall.Exec(path, append([]string{name}, args...), os.Environ())
}

func setupClient(args []string) error {
	if len(args) != 1 || !validName.MatchString(args[0]) {
		return errors.New("usage: anytarts setup HOST, such as anytarts.example.ts.net")
	}
	host := args[0]
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	key := filepath.Join(dir, "id_ed25519")
	if _, err := os.Stat(key); errors.Is(err, os.ErrNotExist) {
		fmt.Println("Making an SSH key. A passphrase keeps it safe if this Mac is stolen.")
		c := exec.Command("ssh-keygen", "-t", "ed25519", "-f", key)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			return fmt.Errorf("ssh-keygen: %w", err)
		}
	} else if err != nil {
		return err
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		return err
	}
	if err := writeSSHConfig(dir, host); err != nil {
		return err
	}
	fmt.Println("SSH is set up.")
	if c, err := net.DialTimeout("tcp", net.JoinHostPort(host, "22"), 5*time.Second); err != nil {
		fmt.Printf("\n%s can't be reached yet. Check that Tailscale is on and you've accepted the invite.\n", host)
	} else {
		c.Close()
		fmt.Printf("%s can be reached.\n", host)
	}
	fmt.Printf("\nIf you're new, send this public key to whoever runs %s:\n\n%s\nThen run: anytarts ssh\n", host, pub)
	return nil
}

// writeSSHConfig keeps our settings in a file of their own, and adds one line
// to the main config to include it. The line goes first because ssh takes the
// first value it finds, and an Include inside a Host block only applies there.
func writeSSHConfig(dir, host string) error {
	own := filepath.Join(dir, "anytarts.conf")
	text := fmt.Sprintf(`# Written by anytarts setup, which replaces it.
Host %[1]s
  HostName %[1]s
  User admin
  ProxyJump %[2]s
  ControlMaster auto
  ControlPath ~/.ssh/cm-%%C
  ControlPersist 5m

Host %[1]s %[2]s
  IdentityFile ~/.ssh/id_ed25519
  ServerAliveInterval 30
  IgnoreUnknown UseKeychain
  AddKeysToAgent yes
  UseKeychain yes
`, vmName, host)
	if err := os.WriteFile(own, []byte(text), 0o600); err != nil {
		return err
	}
	const include = "Include ~/.ssh/anytarts.conf"
	main := filepath.Join(dir, "config")
	old, err := os.ReadFile(main)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if strings.HasPrefix(string(old), include+"\n") {
		return nil
	}
	if len(old) > 0 {
		backup := main + ".before-anytarts"
		f, err := os.OpenFile(backup, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, err = f.Write(old)
			if e := f.Close(); err == nil {
				err = e
			}
		}
		if err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	next := include + "\n"
	if len(old) > 0 {
		next += "\n" + string(old)
	}
	return os.WriteFile(main, []byte(next), 0o600)
}

// screen forwards Screen Sharing to the VM, and stops a little after its last
// connection closes so that a forgotten one doesn't keep the VM on.
func screen() error {
	// Logging in first gets any questions out of the way.
	if err := attached("ssh", vmName, "true").Run(); err != nil {
		return fmt.Errorf("couldn't log in to the VM: %w", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer l.Close()
	url := fmt.Sprintf("vnc://admin@%s", l.Addr())
	if err := exec.Command("open", url).Run(); err != nil {
		return fmt.Errorf("opening Screen Sharing: %w", err)
	}
	fmt.Println("Opening Screen Sharing. Log in as admin with the VM's password.")
	fmt.Println("Leave this running; it stops when you close Screen Sharing.")
	relay(l, 15*time.Second)
	return nil
}

func attached(name string, args ...string) *exec.Cmd {
	c := exec.Command(name, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c
}

// relay returns once there has been a connection and then none for grace.
func relay(l net.Listener, grace time.Duration) {
	var mu sync.Mutex
	open := 0
	var end *time.Timer
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		mu.Lock()
		open++
		if end != nil {
			end.Stop()
		}
		mu.Unlock()
		go func() {
			defer func() {
				mu.Lock()
				defer mu.Unlock()
				if open--; open == 0 {
					end = time.AfterFunc(grace, func() {
						mu.Lock()
						defer mu.Unlock()
						if open == 0 {
							l.Close()
						}
					})
				}
			}()
			defer c.Close()
			ssh := exec.Command("ssh", "-o", "BatchMode=yes", "-W", "localhost:5900", vmName)
			in, err := ssh.StdinPipe()
			if err != nil {
				return
			}
			ssh.Stdout, ssh.Stderr = c, os.Stderr
			if err := ssh.Start(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			go func() {
				io.Copy(in, c)
				in.Close()
				// The viewer has gone; don't wait on a server that hasn't noticed.
				time.Sleep(time.Second)
				ssh.Process.Kill()
			}()
			ssh.Wait()
		}()
	}
}

func screenshot(args []string) error {
	path := "simulator-" + time.Now().Format("20060102-150405") + ".png"
	if len(args) > 1 {
		return errors.New("usage: anytarts screenshot [FILE]")
	}
	if len(args) == 1 {
		path = args[0]
	}
	var out bytes.Buffer
	c := exec.Command("ssh", vmName, "xcrun simctl io booted screenshot --type=png -")
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, &out, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("no screenshot; is a simulator booted in the VM? %w", err)
	}
	if !bytes.HasPrefix(out.Bytes(), []byte("\x89PNG\r\n\x1a\n")) {
		return errors.New("the VM didn't send back a picture")
	}
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		return err
	}
	fmt.Println(path)
	return nil
}

func doctor() error {
	out, err := exec.Command("ssh", "-G", vmName).Output()
	host := ""
	for line := range strings.Lines(string(out)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "proxyjump "); ok {
			host = v[strings.LastIndex(v, "@")+1:]
		}
	}
	if err != nil || host == "" {
		return errors.New("ssh isn't set up for the VM; run anytarts setup HOST")
	}
	fmt.Println("ok    ssh is set up")
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, "22"), 5*time.Second)
	if err != nil {
		return fmt.Errorf("%s can't be reached; check Tailscale is on and the invite was accepted", host)
	}
	c.Close()
	fmt.Printf("ok    %s can be reached\n", host)

	fmt.Println("...   logging in, which starts the VM if it's off")
	var seen bytes.Buffer
	login := exec.Command("ssh", "-o", "BatchMode=yes", vmName, "true")
	login.Stderr = io.MultiWriter(os.Stderr, &seen)
	if err := login.Run(); err != nil {
		return errors.New(diagnose(seen.String()))
	}
	fmt.Println("ok    the VM lets you in")

	for _, check := range []struct{ name, cmd, fix string }{
		{"Xcode works", "xcodebuild -version", "run anytarts ssh xcodebuild -version to see why"},
		{"Screen Sharing is on", "/usr/bin/nc -z localhost 5900", "turn it on in the VM's System Settings, under Sharing"},
		{"the keychain is unlocked", "security show-keychain-info " + keychain, "run anytarts unlock"},
	} {
		if err := exec.Command("ssh", "-o", "BatchMode=yes", vmName, check.cmd).Run(); err != nil {
			fmt.Printf("no    %s: %s\n", check.name, check.fix)
		} else {
			fmt.Printf("ok    %s\n", check.name)
		}
	}
	return nil
}

func diagnose(stderr string) string {
	for _, d := range []struct{ has, means string }{
		{"slots are in use", "CI is using the Mac; try again soon"},
		{"Local Network", "the Mac is blocking anytarts; its owner needs to allow it under Privacy & Security > Local Network"},
		{"REMOTE HOST IDENTIFICATION HAS CHANGED", "the VM or its doorman isn't the one you met before; that's expected after a wipe, otherwise ask its owner"},
		{"Host key verification failed", "you haven't met this VM yet; run anytarts ssh and answer its question"},
		{"Permission denied", "your key wasn't accepted; unlock it with ssh-add, or send your public key to the owner"},
	} {
		if strings.Contains(stderr, d.has) {
			return d.means
		}
	}
	return "couldn't log in; the messages above say why"
}

func printGuide(args []string) error {
	f := flag.NewFlagSet("guide", flag.ContinueOnError)
	install := f.Bool("install", false, "add the guide to Claude Code as a skill")
	if err := f.Parse(args); err != nil {
		return err
	}
	if !*install {
		fmt.Print(guide)
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".claude", "skills", "anytarts", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(guide), 0o644); err != nil {
		return err
	}
	fmt.Println("Added", path)
	return nil
}
