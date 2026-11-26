# anytarts

anytarts starts a [Tart](https://tart.run) VM when its owner connects over SSH,
and shuts it down once they've been disconnected for a while. The VM's disk is
kept.

It joins Tailscale as its own device, so you can share it without sharing the
Mac. It is an SSH jump host with no shell: each key can only reach its own VM,
on ports 22 and 5900.

## Running it

Copy `anytarts.example.toml` to `anytarts.toml`, fill it in, then:

    go build && ./anytarts -config anytarts.toml

The first run prints a Tailscale sign-in link. VMs are kept off the local
network with [softnet](https://github.com/cirruslabs/softnet), and won't start
if it isn't installed. `isolate = false` turns that off.

## Connecting

    Host dev
      User admin
      ProxyJump anytarts

Use `ssh dev`. For Screen Sharing, run `ssh -L 5901:localhost:5900 dev` and open
`vnc://localhost:5901`.

## Good to know

- Run `claude` inside `tmux` so it survives a dropped connection.
- Shutting down closes whatever is running, `tmux` included.
- A change to `image` doesn't touch a VM that already exists. To wipe one, run
  `tart delete dev`; the next connection makes a new copy.
