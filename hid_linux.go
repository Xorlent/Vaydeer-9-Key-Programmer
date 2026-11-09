//go:build linux

package main

// Linux backend. The command channel is USB interface 0 and key events are interface 2.
// sysfs maps nodes to interfaces and supplies the identity strings that openVerifiedDevice checks.
//
// /dev/hidrawN is root-owned by default. Either run as root or install a udev rule - the
// full procedure is in README.md.
//
//	SUBSYSTEM=="hidraw", ATTRS{idVendor}=="0483", ATTRS{idProduct}=="5752", TAG+="uaccess"

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const sysfsHidraw = "/sys/class/hidraw"

const (
	deviceVID = 0x0483
	devicePID = 0x5752
)

type hidrawNode struct {
	path    string
	iface   int // USB bInterfaceNumber: 0 command, 2 key events
	product string
	maker   string
	serial  string
}

func hidrawNodes() ([]hidrawNode, error) {
	entries, err := os.ReadDir(sysfsHidraw)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", sysfsHidraw, err)
	}
	var nodes []hidrawNode
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "hidraw") {
			continue
		}
		if node, ok := describeHidraw(filepath.Join(sysfsHidraw, name), name); ok {
			nodes = append(nodes, node)
		}
	}
	return nodes, nil
}

func describeHidraw(sysDir, name string) (hidrawNode, bool) {
	node := hidrawNode{path: filepath.Join("/dev", name), iface: -1}
	uevent, err := os.ReadFile(filepath.Join(sysDir, "device", "uevent"))
	if err != nil {
		return node, false
	}

	var vid, pid int
	for _, line := range strings.Split(string(uevent), "\n") {
		if rest, ok := strings.CutPrefix(line, "HID_ID="); ok {
			// HID_ID=<bus>:<vid>:<pid>, each 8 hex digits, e.g. 0003:00000483:00005752
			parts := strings.Split(strings.TrimSpace(rest), ":")
			if len(parts) != 3 {
				continue
			}
			if v, err := strconv.ParseUint(parts[1], 16, 32); err == nil {
				vid = int(v)
			}
			if v, err := strconv.ParseUint(parts[2], 16, 32); err == nil {
				pid = int(v)
			}
		} else if rest, ok := strings.CutPrefix(line, "HID_NAME="); ok {
			node.product = strings.TrimSpace(rest)
		}
	}
	if vid != deviceVID || pid != devicePID {
		return node, false
	}

	// .../usb1/1-1/1-1:1.0/0003:0483:5752.0001/hidraw/hidraw0
	//           ^usb dev ^iface ^hid device
	hidDev, err := filepath.EvalSymlinks(filepath.Join(sysDir, "device"))
	if err != nil {
		return node, false
	}
	ifaceDir := filepath.Dir(hidDev)
	iface, err := readInterfaceNumber(ifaceDir)
	if err != nil {
		return node, false
	}
	node.iface = iface
	usbDir := filepath.Dir(ifaceDir)
	node.maker = readTrimmed(filepath.Join(usbDir, "manufacturer"))
	node.serial = readTrimmed(filepath.Join(usbDir, "serial"))
	return node, true
}

// readInterfaceNumber: sysfs reports bInterfaceNumber in hex.
func readInterfaceNumber(dir string) (int, error) {
	if raw, err := os.ReadFile(filepath.Join(dir, "bInterfaceNumber")); err == nil {
		if v, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 16, 32); err == nil {
			return int(v), nil
		}
	}
	// Fall back to the "<bus>-<port>:<config>.<interface>" directory name.
	base := filepath.Base(dir)
	if i := strings.LastIndexByte(base, '.'); i >= 0 {
		if v, err := strconv.Atoi(base[i+1:]); err == nil {
			return v, nil
		}
	}
	return -1, fmt.Errorf("cannot read the USB interface number of %s", dir)
}

func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func nodeByPath(path string) (hidrawNode, bool) {
	nodes, err := hidrawNodes()
	if err != nil {
		return hidrawNode{}, false
	}
	for _, n := range nodes {
		if n.path == path {
			return n, true
		}
	}
	return hidrawNode{}, false
}

func findInterface(kind interfaceKind) (string, error) {
	want := 0
	if kind == ifaceEvent {
		want = 2
	}
	nodes, err := hidrawNodes()
	if err != nil {
		return "", err
	}
	for _, n := range nodes {
		if n.iface == want {
			return n.path, nil
		}
	}
	return "", fmt.Errorf("keypad not found (no hidraw node on USB interface %d for %04X:%04X)",
		want, deviceVID, devicePID)
}

type device struct {
	fd      int // -1 once closed
	path    string
	product string
	maker   string
	serial  string
}

func openDevice(path string) (*device, error) {
	// Non-blocking, otherwise a read blocks forever and the caller's timeout is moot.
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		if err == syscall.EACCES {
			return nil, fmt.Errorf("open %s: %w (needs a udev rule or root)", path, err)
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	d := &device{fd: fd, path: path}
	if node, ok := nodeByPath(path); ok {
		d.product, d.maker, d.serial = node.product, node.maker, node.serial
	}
	return d, nil
}

func (d *device) close() {
	if d.fd >= 0 {
		syscall.Close(d.fd)
	}
	d.fd = -1
}

// io performs one non-blocking transfer, polling until the deadline. hidraw includes the
// report id as the first byte in both directions, which is what the framing layer expects.
func (d *device) io(write bool, buf []byte, timeout time.Duration) (int, error) {
	if d.fd < 0 {
		return 0, errClosed
	}
	deadline := time.Now().Add(timeout)
	for {
		var (
			n   int
			err error
		)
		if write {
			n, err = syscall.Write(d.fd, buf)
		} else {
			n, err = syscall.Read(d.fd, buf)
		}
		switch err {
		case nil:
			if n > 0 || write {
				return n, nil
			}
		case syscall.EAGAIN, syscall.EINTR:
		default:
			return 0, err
		}
		if !time.Now().Before(deadline) {
			return 0, errTimeout
		}
		time.Sleep(time.Millisecond)
	}
}

// flushQueue discards queued input reports; a stale report makes every later read return
// the previous answer. Equivalent to IOCTL_HID_FLUSH_QUEUE on Windows.
func (d *device) flushQueue() {
	if d.fd < 0 {
		return
	}
	buf := make([]byte, reportLen)
	for i := 0; i < 64; i++ {
		n, err := syscall.Read(d.fd, buf)
		if err != nil || n == 0 {
			return
		}
	}
}

func (d *device) Identity() (product, manufacturer, serial string, err error) {
	if d.fd < 0 {
		return "", "", "", errClosed
	}
	return d.product, d.maker, d.serial, nil
}
