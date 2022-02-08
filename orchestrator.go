package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

type CreateRequest struct {
	RootDrivePath string `json:"root_image_path"`
	KernelPath    string `json:"kernel_path"`
}

type CreateResponse struct {
	IpAddress string `json:"ip_address"`
	ID        string `json:"id"`
}

type DeleteRequest struct {
	ID string `json:"id"`
}

var runningVMs map[string]RunningFirecracker = make(map[string]RunningFirecracker)
var ipByte byte = 3

func main() {
	http.HandleFunc("/create", createRequestHandler)
	http.HandleFunc("/delete", deleteRequestHandler)
	defer cleanup()

	log.Fatal(http.ListenAndServe(":8080", nil))
}

func cleanup() {
	for _, running := range runningVMs {
		shutDown(running)
	}
}

func shutDown(running RunningFirecracker) {
	running.machine.StopVMM()
	os.Remove(running.image)
}

func deleteRequestHandler(w http.ResponseWriter, r *http.Request) {
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Fatalf("failed to read body, %s", err)
	}
	var req DeleteRequest
	json.Unmarshal([]byte(body), &req)
	if err != nil {
		log.Fatalf(err.Error())
	}

	running := runningVMs[req.ID]
	shutDown(running)
	delete(runningVMs, req.ID)
}

func putMetadata(unixSocketAddr string, jsonBytes []byte) {
	httpc := http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", unixSocketAddr)
			},
		},
	}
	req, err := http.NewRequest(http.MethodPut, "http://localhost/mmds", bytes.NewReader(jsonBytes))
	req.Header.Set("Content-Type", "application/json")

	_, err = httpc.Do(req)
	if err != nil {
		log.Fatalf("failed to put metadata, %s", err)
	}
}

func createRequestHandler(w http.ResponseWriter, r *http.Request) {
	ipByte += 1
	body, err := ioutil.ReadAll(r.Body)
	log.Println("createRequest " + string(body))
	if err != nil {
		log.Fatalf("failed to read body, %s", err)
	}
	var req CreateRequest
	json.Unmarshal([]byte(body), &req)
	opts := getOptions(ipByte, req)
	running, err := opts.createVMM(context.Background())
	if err != nil {
		log.Fatalf(err.Error())
	}

	id := pseudo_uuid()
	resp := CreateResponse{
		IpAddress: opts.FcIP,
		ID:        id,
	}
	response, err := json.Marshal(&resp)
	if err != nil {
		log.Fatalf("failed to marshal json, %s", err)
	}
	log.Println("createResponse " + string(response))
	w.Header().Add("Content-Type", "application/json")
	w.Write(response)

	runningVMs[id] = *running

	putMetadata(opts.FcSocketPath, body)

	go func() {
		defer running.cancelCtx()
		// there's an error here but we ignore it for now because we terminate
		// the VM on /delete and it returns an error when it's terminated
		running.machine.Wait(running.ctx)
	}()
}

func pseudo_uuid() string {

	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		log.Fatalf("failed to generate uuid, %s", err)
	}

	return fmt.Sprintf("%X-%X-%X-%X-%X", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func getOptions(id byte, req CreateRequest) options {
	fc_ip := net.IPv4(172, 17, 0, id).String()
	gateway_ip := "172.17.0.1"
	docker_mask_long := "255.255.255.0"
	bootArgs := "ro console=ttyS0 noapic reboot=k panic=1 pci=off nomodules random.trust_cpu=on "
	bootArgs = bootArgs + fmt.Sprintf("ip=%s::%s:%s::eth0:off", fc_ip, gateway_ip, docker_mask_long)
	return options{
		//FcBinary:        "/root/release-v1.0.0-x86_64/firecracker-v1.0.0-x86_64",
		FcBinary:		 "/root/release-v0.25.2-x86_64/firecracker-v0.25.2-x86_64",
		Request:         req,
		FcKernelCmdLine: bootArgs,
		FcSocketPath:    fmt.Sprintf("/tmp/firecracker-%d.sock", id),
		TapMacAddr:      fmt.Sprintf("02:FC:00:00:00:%02x", id),
		TapDev:          fmt.Sprintf("fc-tap-%d", id),
		FcIP:            fc_ip,
		FcCPUCount:      1,
		FcMemSz:         512,
	}
}

type RunningFirecracker struct {
	ctx       context.Context
	cancelCtx context.CancelFunc
	image     string
	machine   *firecracker.Machine
}

func (opts *options) createVMM(ctx context.Context) (*RunningFirecracker, error) {
	vmmCtx, vmmCancel := context.WithCancel(ctx)
	rootImagePath, err := copyImage(opts.Request.RootDrivePath)
	opts.Request.RootDrivePath = rootImagePath
	if err != nil {
		return nil, fmt.Errorf("Failed copying root path: %s", err)
	}
	fcCfg, err := opts.getConfig()
	if err != nil {
		return nil, err
	}

	cmd := firecracker.VMCommandBuilder{}.
		WithBin(opts.FcBinary).
		WithSocketPath(fcCfg.SocketPath).
		WithStdin(os.Stdin).
		WithStdout(os.Stdout).
		WithStderr(os.Stderr).
		Build(ctx)

	machineOpts := []firecracker.Opt{
		firecracker.WithProcessRunner(cmd),
	}

	/*
	ip link del "$TAP_DEV" 2> /dev/null || true
	ip tuntap add dev "$TAP_DEV" mode tap
	sysctl -w net.ipv4.conf.${TAP_DEV}.proxy_arp=1 > /dev/null
	sysctl -w net.ipv6.conf.${TAP_DEV}.disable_ipv6=1 > /dev/null
	sudo brctl addif docker0 $TAP_DEV
	ip link set dev "$TAP_DEV" up

	*/
	log.Println("Configuring TAP device: " + opts.TapDev)
	exec.Command("ip", "link", "del", opts.TapDev).Run()
	if err := exec.Command("ip", "tuntap", "add", "dev", opts.TapDev, "mode", "tap").Run(); err != nil {
		return nil, fmt.Errorf("Failed creating ip link: %s", err)
	}
	if err := exec.Command("rm", "-f", opts.FcSocketPath).Run(); err != nil {
		return nil, fmt.Errorf("Failed to delete old socket path: %s", err)
	}
	if err := exec.Command("sysctl", "-w", fmt.Sprintf("net.ipv4.conf.%s.proxy_arp=1", opts.TapDev)).Run(); err != nil {
		return nil, fmt.Errorf("Failed doing first sysctl: %s", err)
	}
	if err := exec.Command("sysctl", "-w", fmt.Sprintf("net.ipv6.conf.%s.disable_ipv6=1", opts.TapDev)).Run(); err != nil {
		return nil, fmt.Errorf("Failed doing second sysctl: %s", err)
	}
	if err := exec.Command("sudo", "brctl", "addif", "docker0", opts.TapDev).Run(); err != nil {
		return nil, fmt.Errorf("Failed doing brctl: %s", err)
	}

	if err := exec.Command("ip", "link", "set", opts.TapDev, "master", "docker0").Run(); err != nil {
	 	return nil, fmt.Errorf("Failed adding tap device to bridge: %s", err)
	}
	if err := exec.Command("ip", "link", "set", "dev", opts.TapDev, "up").Run(); err != nil {
		return nil, fmt.Errorf("Failed creating ip link: %s", err)
	}
	m, err := firecracker.NewMachine(vmmCtx, *fcCfg, machineOpts...)
	if err != nil {
		return nil, fmt.Errorf("Failed creating machine: %s", err)
	}
	if err := m.Start(vmmCtx); err != nil {
		return nil, fmt.Errorf("Failed to start machine: %v", err)
	}
	installSignalHandlers(vmmCtx, m)
	return &RunningFirecracker{
		ctx:       vmmCtx,
		image:     rootImagePath,
		cancelCtx: vmmCancel,
		machine:   m,
	}, nil
}

type options struct {
	Id string `long:"id" description:"Jailer VMM id"`
	// maybe make this an int instead
	IpId            byte   `byte:"id" description:"an ip we use to generate an ip address"`
	FcBinary        string `long:"firecracker-binary" description:"Path to firecracker binary"`
	FcKernelCmdLine string `long:"kernel-opts" description:"Kernel commandline"`
	Request         CreateRequest
	FcSocketPath    string `long:"socket-path" short:"s" description:"path to use for firecracker socket"`
	TapMacAddr      string `long:"tap-mac-addr" description:"tap macaddress"`
	TapDev          string `long:"tap-dev" description:"tap device"`
	FcCPUCount      int64  `long:"ncpus" short:"c" description:"Number of CPUs"`
	FcMemSz         int64  `long:"memory" short:"m" description:"VM memory, in MiB"`
	FcIP            string `long:"fc-ip" description:"IP address of the VM"`
}

func (opts *options) getConfig() (*firecracker.Config, error) {
	drives := []models.Drive{
		models.Drive{
			DriveID:      firecracker.String("1"),
			PathOnHost:   &opts.Request.RootDrivePath,
			IsRootDevice: firecracker.Bool(true),
			IsReadOnly:   firecracker.Bool(false),
		},
	}

	return &firecracker.Config{
		VMID:            opts.Id,
		SocketPath:      opts.FcSocketPath,
		KernelImagePath: opts.Request.KernelPath,
		KernelArgs:      opts.FcKernelCmdLine,
		Drives:          drives,
		NetworkInterfaces: []firecracker.NetworkInterface{
			firecracker.NetworkInterface{
				StaticConfiguration: &firecracker.StaticNetworkConfiguration{
					MacAddress:  opts.TapMacAddr,
					HostDevName: opts.TapDev,
				},
				AllowMMDS: true,
			},
		},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(opts.FcCPUCount),
			MemSizeMib: firecracker.Int64(opts.FcMemSz),
			//CPUTemplate: models.CPUTemplate(opts.FcCPUTemplate),
			HtEnabled: firecracker.Bool(false),
		},
		// // If not provided, the default address (169.254.169.254) will be used.
		//MmdsAddress: "169.254.169.250",
		//JailerCfg: jail,
		//VsockDevices:      vsocks,
		//LogFifo:           opts.FcLogFifo,
		//LogLevel:          opts.FcLogLevel,
		//MetricsFifo:       opts.FcMetricsFifo,
		//FifoLogWriter:     fifo,
	}, nil
}

func copyImage(src string) (string, error) {
	sourceFileStat, err := os.Stat(src)
	if err != nil {
		return "", err
	}

	if !sourceFileStat.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", src)
	}

	source, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer source.Close()

	destination, err := ioutil.TempFile("/images", "image")
	if err != nil {
		return "", err
	}
	defer destination.Close()
	_, err = io.Copy(destination, source)
	return destination.Name(), err
}

func installSignalHandlers(ctx context.Context, m *firecracker.Machine) {
	// not sure if this is actually really helping with anything
	go func() {
		// Clear some default handlers installed by the firecracker SDK:
		signal.Reset(os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)

		for {
			switch s := <-c; {
			case s == syscall.SIGTERM || s == os.Interrupt:
				log.Printf("Caught SIGINT, requesting clean shutdown")
				m.Shutdown(ctx)
			case s == syscall.SIGQUIT:
				log.Printf("Caught SIGTERM, forcing shutdown")
				m.StopVMM()
			}
		}
	}()
}