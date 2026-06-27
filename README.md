# extract-cli

A high-performance, minimalist application lifecycle and package manager for Linux desktop environments written in Go. 

`extract-cli` bridges the gap between raw precompiled archives and native OS desktop integration. It handles decompression, automatic binary/asset discovery, self-healing desktop shortcuts, local tracking manifests, and automated daily background synchronization via systemd.

## Features

* **Multi-Format Extraction Routing:** Inspects binary byte headers to automatically decode ZIP, GZIP, TAR, XZ, ZSTD, and DEB streams without relying on file extensions.
* **Hybrid UX Space:** Supports explicit keyboard path specification or launching a native visual file manager directory picker.
* **Self-Healing Launchers:** Combats Electron application background singleton lock freezes by cleanly appending targeted process name management (`pkill -9 -x`) straight into native `.desktop` files.
* **Stateful Local Manifests:** Drops hidden JSON tracking manifests (`.extract-meta.json`) into target application workspaces when deploying software directly from remote URLs.
* **Automated Background Updates:** Interfaces with the Linux core via lightweight user-space systemd services and calendar timers for automated daily synchronization.

## Installation

Ensure Go is installed and configured in your environment system path variables, then run:

```bash
go install github.com/ShadiSairafi/extract-cli@latest
