//go:build darwin

package main

// Darwin backend over IOKit's HID Manager (cgo). I/O on one pinned OS thread for reliability.
// see writeReport for a SetReport quirk and findInterface for interface selection.

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <IOKit/hid/IOHIDManager.h>
#include <IOKit/hid/IOHIDKeys.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>
#include <string.h>

#define KM_MAX_REPORTS 64
#define KM_REPORT_MAX  66 // report id + 65 bytes

typedef struct {
	uint32_t locationID;
	int usagePage;
	int usage;
	char product[128];
	char manufacturer[128];
	char serial[128];
} km_device_info;

typedef struct {
	uint8_t buf[KM_MAX_REPORTS][KM_REPORT_MAX];
	int len[KM_MAX_REPORTS];
	volatile int head;
	volatile int tail;
} km_ring;

// Single-producer (input callback) / single-consumer (io) ring accessors.
static int km_ring_head(km_ring *r) {
	return __atomic_load_n(&r->head, __ATOMIC_ACQUIRE);
}

static void km_ring_consume(km_ring *r, int idx) {
	__atomic_store_n(&r->tail, idx, __ATOMIC_RELEASE);
}

static IOHIDManagerRef km_manager = NULL;

static int km_int_prop(IOHIDDeviceRef dev, CFStringRef key, int dflt) {
	int out = dflt;
	CFTypeRef v = IOHIDDeviceGetProperty(dev, key);
	if (v && CFGetTypeID(v) == CFNumberGetTypeID()) {
		CFNumberGetValue((CFNumberRef)v, kCFNumberIntType, &out);
	}
	return out;
}

static void km_str_prop(IOHIDDeviceRef dev, CFStringRef key, char *dst, size_t cap) {
	dst[0] = '\0';
	CFTypeRef v = IOHIDDeviceGetProperty(dev, key);
	if (v && CFGetTypeID(v) == CFStringGetTypeID()) {
		CFStringGetCString((CFStringRef)v, dst, (CFIndex)cap, kCFStringEncodingUTF8);
	}
}

// One process-lifetime manager keeps the returned IOHIDDeviceRef values alive.
static IOHIDManagerRef km_get_manager(void) {
	if (km_manager) {
		return km_manager;
	}
	km_manager = IOHIDManagerCreate(kCFAllocatorDefault, kIOHIDOptionsTypeNone);
	if (!km_manager) {
		return NULL;
	}
	CFMutableDictionaryRef match = CFDictionaryCreateMutable(kCFAllocatorDefault, 0,
		&kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	if (match) {
		int vid = 0x0483, pid = 0x5752, vendorUsagePage = 0xFF00;
		CFNumberRef nv = CFNumberCreate(kCFAllocatorDefault, kCFNumberIntType, &vid);
		CFNumberRef np = CFNumberCreate(kCFAllocatorDefault, kCFNumberIntType, &pid);
		CFNumberRef nup = CFNumberCreate(kCFAllocatorDefault, kCFNumberIntType, &vendorUsagePage);
		if (nv) {
			CFDictionarySetValue(match, CFSTR(kIOHIDVendorIDKey), nv);
			CFRelease(nv);
		}
		if (np) {
			CFDictionarySetValue(match, CFSTR(kIOHIDProductIDKey), np);
			CFRelease(np);
		}
		if (nup) {
			// Restrict to the vendor page (0xFF00) so the keyboard/mouse interfaces are
			// never opened, which would trigger the Input Monitoring prompt.
			CFDictionarySetValue(match, CFSTR(kIOHIDDeviceUsagePageKey), nup);
			CFRelease(nup);
		}
		IOHIDManagerSetDeviceMatching(km_manager, match);
		CFRelease(match);
	}
	return km_manager;
}

static void **km_copy_devices(CFIndex *count) {
	void **vals = NULL;
	IOHIDManagerRef mgr = km_get_manager();
	*count = 0;
	if (!mgr) {
		return NULL;
	}
	CFSetRef set = IOHIDManagerCopyDevices(mgr);
	if (!set) {
		return NULL;
	}
	CFIndex n = CFSetGetCount(set);
	if (n > 0) {
		vals = (void **)calloc((size_t)n, sizeof(void *));
		if (vals) {
			CFSetGetValues(set, (const void **)vals);
			*count = n;
		}
	}
	CFRelease(set);
	return vals;
}

static int km_list(km_device_info *out, int max) {
	CFIndex n = 0;
	void **vals = km_copy_devices(&n);
	if (!vals || !out || max <= 0) {
		free(vals);
		return 0;
	}
	int count = 0;
	for (CFIndex i = 0; i < n && count < max; i++) {
		IOHIDDeviceRef dev = (IOHIDDeviceRef)vals[i];
		if (!dev) {
			continue;
		}
		km_device_info *d = &out[count++];
		d->locationID = (uint32_t)km_int_prop(dev, CFSTR(kIOHIDLocationIDKey), 0);
		d->usagePage = km_int_prop(dev, CFSTR(kIOHIDPrimaryUsagePageKey), 0);
		d->usage = km_int_prop(dev, CFSTR(kIOHIDPrimaryUsageKey), 0);
		km_str_prop(dev, CFSTR(kIOHIDProductKey), d->product, sizeof(d->product));
		km_str_prop(dev, CFSTR(kIOHIDManufacturerKey), d->manufacturer, sizeof(d->manufacturer));
		km_str_prop(dev, CFSTR(kIOHIDSerialNumberKey), d->serial, sizeof(d->serial));
	}
	free(vals);
	return count;
}

static IOHIDDeviceRef km_find(uint32_t locationID, int usagePage, int usage) {
	CFIndex n = 0;
	void **vals = km_copy_devices(&n);
	if (!vals) {
		return NULL;
	}
	IOHIDDeviceRef found = NULL;
	for (CFIndex i = 0; i < n; i++) {
		IOHIDDeviceRef dev = (IOHIDDeviceRef)vals[i];
		if (dev &&
		    (uint32_t)km_int_prop(dev, CFSTR(kIOHIDLocationIDKey), 0) == locationID &&
		    km_int_prop(dev, CFSTR(kIOHIDPrimaryUsagePageKey), 0) == usagePage &&
		    km_int_prop(dev, CFSTR(kIOHIDPrimaryUsageKey), 0) == usage) {
			found = dev;
			break;
		}
	}
	free(vals);
	return found;
}

static km_ring *km_ring_new(void) {
	return (km_ring *)calloc(1, sizeof(km_ring));
}

static void km_input_cb(void *ctx, IOReturn result, void *sender, IOHIDReportType type,
                        uint32_t reportID, uint8_t *report, CFIndex length) {
	km_ring *ring = (km_ring *)ctx;
	if (!ring || result != kIOReturnSuccess) {
		return;
	}
	int head = __atomic_load_n(&ring->head, __ATOMIC_RELAXED);
	int next = (head + 1) % KM_MAX_REPORTS;
	if (next == __atomic_load_n(&ring->tail, __ATOMIC_ACQUIRE)) {
		return; // full: drop rather than block the run loop
	}
	int len = (int)length;
	if (len > KM_REPORT_MAX - 1) {
		len = KM_REPORT_MAX - 1;
	}
	ring->buf[head][0] = (uint8_t)reportID;
	if (len > 0 && report) {
		memcpy(&ring->buf[head][1], report, (size_t)len);
	}
	ring->len[head] = len + 1;
	__atomic_store_n(&ring->head, next, __ATOMIC_RELEASE);
}

// Must be called from the goroutine that will pump the run loop.
static int km_open(IOHIDDeviceRef dev, km_ring *ring) {
	if (!dev) {
		return -1;
	}
	IOReturn rc = IOHIDDeviceOpen(dev, kIOHIDOptionsTypeNone);
	if (rc != kIOReturnSuccess) {
		return (int)rc;
	}
	// The callback buffer must outlive the registration, so it is never freed: the
	// process is short lived and the size is fixed.
	uint8_t *buf = (uint8_t *)calloc(KM_REPORT_MAX, 1);
	if (!buf) {
		IOHIDDeviceClose(dev, kIOHIDOptionsTypeNone);
		return -2;
	}
	IOHIDDeviceRegisterInputReportCallback(dev, buf, (CFIndex)KM_REPORT_MAX,
	                                       (IOHIDReportCallback)km_input_cb, ring);
	IOHIDDeviceScheduleWithRunLoop(dev, CFRunLoopGetCurrent(), kCFRunLoopDefaultMode);
	return 0;
}

static void km_pump(double seconds) {
	CFRunLoopRunInMode(kCFRunLoopDefaultMode, (CFTimeInterval)seconds, true);
}

static int km_write(IOHIDDeviceRef dev, const uint8_t *data, int len) {
	if (!dev || len < 1) {
		return -1;
	}
	// IOKit takes the report id separately and the body without it.
	IOReturn rc = IOHIDDeviceSetReport(dev, kIOHIDReportTypeOutput,
	                                   (CFIndex)data[0], data + 1, (CFIndex)(len - 1));
	return (int)rc;
}

static void km_shutdown(IOHIDDeviceRef dev) {
	if (dev) {
		IOHIDDeviceUnscheduleFromRunLoop(dev, CFRunLoopGetCurrent(), kCFRunLoopDefaultMode);
		IOHIDDeviceClose(dev, kIOHIDOptionsTypeNone);
	}
}
*/
import "C"

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"
)

const usagePageVendor = 0xFF00

type devInfo struct {
	locationID       uint32
	usagePage, usage int
	product          string
	maker            string
	serial           string
}

func cStr(arr *[128]C.char) string { return C.GoString(&arr[0]) }

func listDevices() []devInfo {
	buf := make([]C.km_device_info, 8)
	n := int(C.km_list((*C.km_device_info)(unsafe.Pointer(&buf[0])), C.int(len(buf))))
	if n <= 0 {
		return nil
	}
	out := make([]devInfo, 0, n)
	for _, d := range buf[:n] {
		out = append(out, devInfo{
			locationID: uint32(d.locationID),
			usagePage:  int(d.usagePage),
			usage:      int(d.usage),
			product:    cStr(&d.product),
			maker:      cStr(&d.manufacturer),
			serial:     cStr(&d.serial),
		})
	}
	return out
}

func findInterface(kind interfaceKind) (string, error) {
	wantUsage := 1
	if kind == ifaceEvent {
		wantUsage = 2
	}
	for _, d := range listDevices() {
		if d.usagePage == usagePageVendor && d.usage == wantUsage {
			// The four interfaces share one locationID, so pin page and usage in the path.
			return fmt.Sprintf("iokit:%08X:%04X:%04X", d.locationID, d.usagePage, d.usage), nil
		}
	}
	return "", fmt.Errorf("keypad not found (no IOHIDDevice with usage page 0x%04X usage %d)",
		usagePageVendor, wantUsage)
}

func parseIokitPath(path string) (uint32, int, int, error) {
	rest, ok := strings.CutPrefix(path, "iokit:")
	if !ok {
		return 0, 0, 0, fmt.Errorf("unexpected device path %q", path)
	}
	parts := strings.Split(rest, ":")
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("unexpected device path %q", path)
	}
	loc, err := strconv.ParseUint(parts[0], 16, 32)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("unexpected device path %q: %w", path, err)
	}
	page, err := strconv.ParseUint(parts[1], 16, 16)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("unexpected device path %q: %w", path, err)
	}
	usage, err := strconv.ParseUint(parts[2], 16, 16)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("unexpected device path %q: %w", path, err)
	}
	return uint32(loc), int(page), int(usage), nil
}

// kmMaxReports mirrors KM_MAX_REPORTS in the preamble.
const kmMaxReports = 64

const ringPollInterval = 0.02

// writeReq posts an output report to the I/O goroutine; SetReport must run on the
// thread that opened the device.
type writeReq struct {
	data []byte
	err  chan error
}

type device struct {
	ref     C.IOHIDDeviceRef
	ring    *C.km_ring
	closed  int32 // atomic
	done    chan struct{}
	readIdx int
	writes  chan writeReq
	product string
	maker   string
	serial  string
}

func openDevice(path string) (*device, error) {
	loc, page, usage, err := parseIokitPath(path)
	if err != nil {
		return nil, err
	}

	var meta devInfo
	found := false
	for _, d := range listDevices() {
		if d.locationID == loc && d.usagePage == page && d.usage == usage {
			meta, found = d, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("keypad at %s is no longer present", path)
	}

	ref := C.km_find(C.uint32_t(loc), C.int(page), C.int(usage))
	if ref == 0 {
		return nil, fmt.Errorf("keypad at %s could not be resolved", path)
	}
	ring := C.km_ring_new()
	if ring == nil {
		return nil, fmt.Errorf("out of memory allocating the report ring")
	}

	d := &device{ref: ref, ring: ring, done: make(chan struct{}), writes: make(chan writeReq), product: meta.product, maker: meta.maker, serial: meta.serial}

	// Open, schedule and close on the one goroutine pinned to the run loop.
	opened := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if code := C.km_open(ref, ring); code != 0 {
			C.free(unsafe.Pointer(ring))
			opened <- fmt.Errorf("IOHIDDeviceOpen failed (IOReturn 0x%08X)", uint32(int32(code)))
			return
		}
		opened <- nil
		for atomic.LoadInt32(&d.closed) == 0 {
			// Run SetReport on the pinned run-loop thread, not the caller's.
			select {
			case req := <-d.writes:
				req.err <- writeReport(ref, req.data)
			default:
			}
			C.km_pump(C.double(ringPollInterval))
		}
		C.km_shutdown(ref)
		C.free(unsafe.Pointer(ring))
		close(d.done)
	}()
	if err := <-opened; err != nil {
		return nil, err
	}
	return d, nil
}

func (d *device) close() {
	if atomic.SwapInt32(&d.closed, 1) != 0 {
		return
	}
	<-d.done
}

// writeReport sends one output report from the pinned I/O goroutine.
func writeReport(ref C.IOHIDDeviceRef, buf []byte) error {
	if len(buf) == 0 {
		return fmt.Errorf("empty output report")
	}
	code := C.km_write(ref, (*C.uint8_t)(unsafe.Pointer(&buf[0])), C.int(len(buf)))
	if code == 0 {
		return nil
	}
	// Newer SDKs return kIOReturnError spuriously even when the report is delivered;
	// the read that follows catches a genuinely lost report.
	if uint32(int32(code)) == 0xE00002BC {
		return nil
	}
	return fmt.Errorf("IOHIDDeviceSetReport failed (IOReturn 0x%08X)", uint32(int32(code)))
}

func (d *device) io(write bool, buf []byte, timeout time.Duration) (int, error) {
	if atomic.LoadInt32(&d.closed) != 0 {
		return 0, errClosed
	}
	if write {
		req := writeReq{data: buf, err: make(chan error, 1)}
		select {
		case d.writes <- req:
		case <-d.done:
			return 0, errClosed
		}
		if err := <-req.err; err != nil {
			return 0, err
		}
		return len(buf), nil
	}
	deadline := time.Now().Add(timeout)
	for {
		if head := int(C.km_ring_head(d.ring)); d.readIdx != head {
			n := int(d.ring.len[d.readIdx])
			if n > len(buf) {
				n = len(buf)
			}
			src := unsafe.Slice((*byte)(unsafe.Pointer(&d.ring.buf[d.readIdx][0])), n)
			copy(buf[:n], src)
			d.readIdx = (d.readIdx + 1) % kmMaxReports
			C.km_ring_consume(d.ring, C.int(d.readIdx))
			return n, nil
		}
		if atomic.LoadInt32(&d.closed) != 0 {
			return 0, errClosed
		}
		if !time.Now().Before(deadline) {
			return 0, errTimeout
		}
		time.Sleep(time.Millisecond)
	}
}

// Drops queued input reports (cf. IOCTL_HID_FLUSH_QUEUE on Windows).
func (d *device) flushQueue() {
	d.readIdx = int(C.km_ring_head(d.ring))
	C.km_ring_consume(d.ring, C.int(d.readIdx))
}

func (d *device) Identity() (product, manufacturer, serial string, err error) {
	if atomic.LoadInt32(&d.closed) != 0 {
		return "", "", "", errClosed
	}
	return d.product, d.maker, d.serial, nil
}