# Port forwarding
```bash
ssh -L 8080:localhost:8080 user@your-laptop        #for one time forward

autossh -M 0 -fNT \
  -o ServerAliveInterval=30 \
  -o ServerAliveCountMax=3 \
  -R 9000:localhost:8080 \
  user@remote-host                              #for auto restart and keep port open at all  times

```
# Bidder Watcher
Go service that polls the Succinct dashboard for bidder tuning parameters and rewrites the bidder’s `.env` when any values change. After every successful update it reloads systemd and restarts the bidder service so the new configuration is live immediately.

## Prerequisites
- Go 1.21+ (for local development)
- Access to the dashboard endpoint that emits `small_bid`, `large_bid`, and `max_concurrency`
- Read/write access to the target `.env` (default `~/sp1-cluster/infra/.env`)
- Permission to run `sudo systemctl daemon-reload` and `sudo systemctl restart bidder` on the host

## Local Development & Testing
```bash
# 1. (Optional) Run the mock config service
go run mock.go

# 2. Run the watcher against the mock endpoint
go run main.go \
  -endpoint=http://localhost:8080/config \
  -interval=30s \
  -env=.env \
  -dry-run
```

- `-endpoint` – HTTP URL to poll (default `http://localhost:8080/config`)
- `-interval` – Polling cadence (accepts Go duration strings like `30s`, `1m`)
- `-env` – Path to the `.env` file to rewrite; expand `~` paths automatically
- `-dry-run` – When `true`, skips the `systemctl` commands but still performs file updates, logging the commands it would run

### Building a Binary
```bash
go build -o bidder-watcher main.go
# run it
./bidder-watcher -endpoint=https://dashboard.example/config -interval=30s -env=~/sp1-cluster/infra/.env
```

## Deploying with systemd
1. Copy the binary to the host:
   ```bash
   scp bidder-watcher user@host:/home/user/bin/
   ssh user@host 'sudo mv /home/user/bin/bidder-watcher /usr/local/bin/bidder-watcher && sudo chmod 755 /usr/local/bin/bidder-watcher'
   ```
2. Ensure the target `.env` exists (e.g. `/home/user/sp1-cluster/infra/.env`) and that the service user can edit it.
3. Install the systemd unit (`bidder-watcher.service` in this repo is a template—update paths, endpoint, and `User`/`Group`):
   ```bash
   sudo cp bidder-watcher.service /etc/systemd/system/bidder-watcher.service
   sudo systemctl daemon-reload
   sudo systemctl enable --now bidder-watcher
   ```
4. Confirm it is running:
   ```bash
   systemctl status bidder-watcher
   journalctl -u bidder-watcher -f   # live logs
   ```

### Updating the Service
```bash
scp bidder-watcher user@host:/tmp/
ssh user@host '
  sudo mv /tmp/bidder-watcher /usr/local/bin/bidder-watcher &&
  sudo chmod 755 /usr/local/bin/bidder-watcher &&
  sudo systemctl restart bidder-watcher
'
```
If you edit the unit file, run `sudo systemctl daemon-reload` before restarting.

## Troubleshooting
- “command not found: sudo/systemctl”: run on a Linux host with systemd, or disable `-dry-run` only when those commands are available.
- No updates detected: verify the endpoint returns fields, the process logs payload changes, and the `.env` path matches the host’s file.

## Mock Endpoint
`mock.go` exposes `/config` (current values) and `/set?small_bid=x&large_bid=y&max_concurrency=z` to simulate dashboard pushes during development.
