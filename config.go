package main

import (
	"fmt"
	"slices"
	"time"

	"github.com/BurntSushi/toml"
	"golang.org/x/crypto/ssh"
)

type Config struct {
	Hostname string              `toml:"hostname"`
	StateDir string              `toml:"state_dir"`
	Tart     string              `toml:"tart"`
	MaxVMs   int                 `toml:"max_vms"`
	VMs      map[string]VMConfig `toml:"vm"`
}

type VMConfig struct {
	Image    string   `toml:"image"`
	Keys     []string `toml:"keys"`
	CPU      int      `toml:"cpu"`
	MemoryGB int      `toml:"memory_gb"`
	Idle     duration `toml:"idle"`
	Ports    []uint32 `toml:"ports"`
	// Isolate keeps the VM off the local network with softnet. It is on unless
	// the file turns it off, and a VM that wants it won't start without it.
	Isolate *bool `toml:"isolate"`
}

func (c VMConfig) isolated() bool { return c.Isolate == nil || *c.Isolate }

func (c VMConfig) allows(port uint32) bool { return slices.Contains(c.Ports, port) }

type duration time.Duration

func (d *duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	*d = duration(v)
	return err
}

func loadConfig(path string) (*Config, map[string]string, error) {
	c := &Config{Hostname: "anytarts", Tart: "tart", MaxVMs: 2}
	md, err := toml.DecodeFile(path, c)
	if err != nil {
		return nil, nil, err
	}
	if extra := md.Undecoded(); len(extra) > 0 {
		return nil, nil, fmt.Errorf("unknown setting %s", extra[0])
	}
	if c.StateDir == "" {
		return nil, nil, fmt.Errorf("state_dir is required")
	}
	keys := map[string]string{}
	for name, vm := range c.VMs {
		if vm.Image == "" || len(vm.Keys) == 0 {
			return nil, nil, fmt.Errorf("vm.%s needs an image and at least one key", name)
		}
		if vm.Idle <= 0 {
			vm.Idle = duration(30 * time.Minute)
		}
		if len(vm.Ports) == 0 {
			vm.Ports = []uint32{22, 5900}
		}
		for _, line := range vm.Keys {
			k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
			if err != nil {
				return nil, nil, fmt.Errorf("vm.%s key: %w", name, err)
			}
			id := string(k.Marshal())
			if other, taken := keys[id]; taken && other != name {
				return nil, nil, fmt.Errorf("the same key is on vm.%s and vm.%s", other, name)
			}
			keys[id] = name
		}
		c.VMs[name] = vm
	}
	return c, keys, nil
}
