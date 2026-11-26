package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Hypervisor interface {
	// State says whether the VM exists and is running, and how many VMs are
	// running in all, CI's included.
	State(ctx context.Context, name string) (exists, running bool, total int, err error)
	Create(ctx context.Context, name string, c VMConfig) error
	Start(ctx context.Context, name string, c VMConfig) error
	IP(ctx context.Context, name string) (string, error)
	Stop(ctx context.Context, name string) error
}

// isolation keeps a VM off the local network. softnet alone still lets a VM
// open connections to the Mac it runs on, so that is blocked too, apart from
// DNS lookups. The Mac can still connect in, as these rules are one-way.
var isolation = []string{
	"--net-softnet",
	"--net-softnet-block=out @host",
	"--net-softnet-allow=out proto udp @host port 53",
}

type tart struct{ bin, logDir string }

func (t tart) run(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, t.bin, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("tart %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (t tart) State(ctx context.Context, name string) (exists, running bool, total int, err error) {
	out, err := t.run(ctx, "list", "--source", "local", "--format", "json")
	if err != nil {
		return
	}
	var vms []struct {
		Name    string
		Running bool
	}
	if err = json.Unmarshal([]byte(out), &vms); err != nil {
		return
	}
	for _, vm := range vms {
		if vm.Running {
			total++
		}
		if vm.Name == name {
			exists, running = true, vm.Running
		}
	}
	return
}

func (t tart) Create(ctx context.Context, name string, c VMConfig) error {
	if _, err := t.run(ctx, "clone", c.Image, name); err != nil {
		return err
	}
	return nil
}

func (t tart) Start(ctx context.Context, name string, c VMConfig) error {
	set := []string{"set", name}
	if c.CPU > 0 {
		set = append(set, "--cpu", strconv.Itoa(c.CPU))
	}
	if c.MemoryGB > 0 {
		set = append(set, "--memory", strconv.Itoa(c.MemoryGB*1024))
	}
	if len(set) > 2 {
		if _, err := t.run(ctx, set...); err != nil {
			return err
		}
	}
	args := []string{"run", name, "--no-graphics"}
	if c.isolated() {
		if _, err := exec.LookPath("softnet"); err != nil {
			return fmt.Errorf("softnet isn't installed, so %s can't be isolated: %w", name, err)
		}
		args = append(args, isolation...)
	}
	log, err := os.OpenFile(filepath.Join(t.logDir, name+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	// Its own session, so restarting anytarts leaves the VM running.
	cmd := exec.Command(t.bin, args...)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

func (t tart) IP(ctx context.Context, name string) (string, error) {
	return t.run(ctx, "ip", "--resolver", "agent", name)
}

// Stop asks macOS to shut down, so open files are saved, and only pulls the
// plug if it hasn't after a minute.
func (t tart) Stop(ctx context.Context, name string) error {
	ask, cancel := context.WithTimeout(ctx, 10*time.Second)
	t.run(ask, "exec", name, "sudo", "-n", "/sbin/shutdown", "-h", "now")
	cancel()
	for range 30 {
		_, running, _, err := t.State(ctx, name)
		if err == nil && !running {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	_, err := t.run(ctx, "stop", name, "--timeout", "30")
	return err
}
