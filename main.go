// anytarts starts a Tart VM when its owner connects over SSH and shuts it down
// once they've been gone a while.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	"tailscale.com/tsnet"
)

func main() {
	path := flag.String("config", "anytarts.toml", "config file")
	local := flag.String("listen", "", "listen on this address instead of Tailscale, for testing")
	flag.Parse()
	if err := run(*path, *local); err != nil {
		log.Fatal(err)
	}
}

func run(path, local string) error {
	cfg, keys, err := loadConfig(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return err
	}
	hostKey, err := loadHostKey(cfg.StateDir)
	if err != nil {
		return err
	}
	dial := func(ctx context.Context, addr string) (net.Conn, error) {
		d := net.Dialer{Timeout: 3 * time.Second}
		return d.DialContext(ctx, "tcp", addr)
	}
	vms := map[string]*VM{}
	for name, c := range cfg.VMs {
		vm := &VM{
			name: name, cfg: c, max: cfg.MaxVMs, hv: tart{cfg.Tart, cfg.StateDir}, dial: dial,
			poll: 2 * time.Second, tick: 5 * time.Second, bootTimeout: 3 * time.Minute,
		}
		vms[name] = vm
		// Start the clock on a VM left running by an earlier run.
		vm.acquire()
		vm.release()
	}

	var l net.Listener
	if local != "" {
		l, err = net.Listen("tcp", local)
	} else {
		ts := &tsnet.Server{Hostname: cfg.Hostname, Dir: filepath.Join(cfg.StateDir, "tailscale"), UserLogf: log.Printf}
		defer ts.Close()
		l, err = ts.Listen("tcp", ":22")
	}
	if err != nil {
		return err
	}
	log.Printf("listening on %s", l.Addr())
	return newServer(vms, keys, hostKey).serve(l)
}
