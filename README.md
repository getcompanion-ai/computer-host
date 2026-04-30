## computer-host

<img width=auto height=auto alt="Gemini_Generated_Image_d10lsxd10lsxd10l" src="https://github.com/user-attachments/assets/2f422d7e-0e17-4106-826a-0516f1edc828" />


computer-host is a daemon runtime for managing Firecracker microVMs
on bare-metal Linux hosts. It talks directly to the Firecracker HTTP
API via jailer, exposing a JSON interface over a Unix socket.

It is intentionally synchronous. Firecracker boots a VM in under 200ms -
the overhead of an async job queue would dwarf the actual work. State is
a JSON file on disk. Logs go to the journal.

The official Firecracker Go SDK has been unmaintained for months and
wraps too little of the lifecycle to be useful here. computer-host talks
directly to the Firecracker HTTP API over a Unix socket, manages tap
devices and nftables rules for networking, handles SSH key generation,
guest identity injection, and disk snapshots - all as atomic operations
behind a single host-level contract.

### API

All endpoints accept and return JSON over Unix socket.

```
GET    /health                          health check
POST   /machines                        create a machine
GET    /machines                        list all machines
GET    /machines/{id}                   get machine by id
DELETE /machines/{id}                   delete a machine
POST   /machines/{id}/stop              stop a running machine
POST   /machines/{id}/snapshots         snapshot a running machine
GET    /machines/{id}/snapshots          list snapshots for a machine
GET    /snapshots/{id}                  get snapshot by id
DELETE /snapshots/{id}                  delete a snapshot
POST   /snapshots/{id}/restore          restore snapshot to a new machine
```

### Running

Requires a Linux host with KVM, Firecracker, and jailer installed.

```
export FIRECRACKER_HOST_ROOT_DIR=/var/lib/computer-host
export FIRECRACKER_BINARY_PATH=/usr/local/bin/firecracker
export JAILER_BINARY_PATH=/usr/local/bin/jailer
export FIRECRACKER_HOST_EGRESS_INTERFACE=eth0

go build -o computer-host .
sudo ./computer-host
```

The daemon listens on `$FIRECRACKER_HOST_ROOT_DIR/firecracker-host.sock`.

```
curl --unix-socket /var/lib/computer-host/firecracker-host.sock \
  http://localhost/health
```


While some issues are resolved with github this repo is maintained [here](https://git.harivan.sh/AgentComputerAI/computer-host/issues)
