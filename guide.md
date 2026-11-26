---
name: anytarts
description: Build, run and test iOS apps on the user's remote macOS VM. Use when a task needs Xcode, xcodebuild, simctl or an iPhone simulator.
---

# anytarts

The user has a macOS VM with Xcode. You run on their own Mac and drive the VM
over SSH. The code and its git checkout live in the VM.

- Run a command: `anytarts ssh 'cd ~/app && xcodebuild -scheme App build'`.
  Put the whole remote command in single quotes. It exits with the command's own
  status.
- The VM starts by itself. If it was off, allow a few minutes before timing out.
  Lines starting `[anytarts]` are progress on stderr, not errors.
- Each command is a new shell, so `cd` in each one.
- The VM turns off a while after the last connection closes. Keep long builds in
  the foreground so they hold a connection open.
- Copy files with `scp FILE dev:PATH` and `scp dev:PATH FILE`.
- Boot a simulator with `anytarts ssh 'xcrun simctl boot "iPhone 18 Pro"'`. List
  what's there with `anytarts ssh 'xcrun simctl list devices available'`.
- Look at the simulator with `anytarts screenshot shot.png`, then read the file.
- Commit and push work often. Only what's on GitHub survives the VM being wiped.
- If something can't connect, run `anytarts doctor` and tell the user what it
  says.
- `anytarts screen` and `anytarts unlock` need the user, so ask them to run those.
  Never ask for the VM's password or put it in a command.
