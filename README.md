# cronbon-orchestrator

## Dependencies

```bash
sudo apt install bridge-utils
```

## Kernel and Root Image

```bash
mkdir -p run
mkdir -p run/images
curl -L -o run/vmlinux https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.10/x86_64/vmlinux-5.10.223
curl -L -o run/rootfs https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.10/x86_64/ubuntu-22.04.ext4
```

## Firecracker binary

```bash
curl -L https://github.com/firecracker-microvm/firecracker/releases/download/v1.9.1/firecracker-v1.9.1-x86_64.tgz | tar -zx -C run
ln -s release-v1.9.1-x86_64/firecracker-v1.9.1-x86_64 run/firecracker
```

## Build and run the orchestrator

```bash
go mod download
go build
sudo ./cronbon-orchestrator 
```

## Start a new VM

```bash
curl -X POST http://localhost:8080/create -H 'Content-Type: application/json' -d '{"kernel_path": "./run/vmlinux","root_image_path": "./run/rootfs"}'
```