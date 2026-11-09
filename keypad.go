package main

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// reportLen is the full HID report: id, opcode, length, payload, checksum.
	reportLen = 65

	opGetInfo      = 0x60
	opSetKey       = 0x61
	opGetKey       = 0x62
	opLayerHeader  = 0x65
	opCommitLayer  = 0x66
	opGetLayerName = 0x67
	opKeyEvent     = 0xFB

	vendorVIDPID = "VID_0483&PID_5752"

	// 0483:5752 is a generic STM32 custom-HID pair. Verify with "9-key" and "vydeer" match.
	productFrag      = "9-key"
	manufacturerFrag = "vaydeer"

	keyCount   = 9
	chunkSize  = 60
	flushDelay = 10 * time.Millisecond
	ioTimeout  = 2 * time.Second

	maxPayload = reportLen - 4
)

var (
	errTimeout = errors.New("timeout waiting for the device")
	errClosed  = errors.New("device handle is closed")
)

// interfaceKind names a HID interface; each OS maps it differently
type interfaceKind int

const (
	ifaceCommand interfaceKind = iota // request/response channel
	ifaceEvent                        // asynchronous key events
)

// Keypad serializes access because a transaction spans several frames. Exported
// methods lock and lower-case helpers assume an existing lock.
type Keypad struct {
	mu  sync.Mutex
	dev *device
}

func Open() (*Keypad, error) { return openKeypad(ifaceCommand) }

func OpenEventChannel() (*Keypad, error) { return openKeypad(ifaceEvent) }

func openKeypad(kind interfaceKind) (*Keypad, error) {
	path, err := findInterface(kind)
	if err != nil {
		return nil, err
	}
	d, err := openVerifiedDevice(path)
	if err != nil {
		return nil, err
	}
	return &Keypad{dev: d}, nil
}

func identityMatches(product, manufacturer string) bool {
	return strings.Contains(strings.ToLower(product), productFrag) &&
		strings.Contains(strings.ToLower(manufacturer), manufacturerFrag)
}

func (d *device) verifyIdentity() error {
	product, manufacturer, _, err := d.Identity()
	if err != nil {
		return err
	}
	if !identityMatches(product, manufacturer) {
		return fmt.Errorf("not a Vaydeer 9-key keypad (product %q, manufacturer %q)",
			product, manufacturer)
	}
	return nil
}

func openVerifiedDevice(path string) (*device, error) {
	d, err := openDevice(path)
	if err != nil {
		return nil, err
	}
	if err := d.verifyIdentity(); err != nil {
		d.close()
		return nil, err
	}
	return d, nil
}

func (k *Keypad) Close() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.dev.close()
}

func (k *Keypad) Identity() (product, manufacturer, serial string, err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.dev.Identity()
}

func xorSum(b []byte) byte {
	var c byte
	for _, v := range b {
		c ^= v
	}
	return c
}

// encodeFrame: byte 0 must be the report id, otherwise the device reads the opcode
// as one.
func encodeFrame(opcode byte, payload []byte) ([]byte, error) {
	if len(payload) > maxPayload {
		return nil, fmt.Errorf("payload of %d bytes exceeds frame capacity %d",
			len(payload), maxPayload)
	}
	body := make([]byte, 0, 2+len(payload))
	body = append(body, opcode, byte(len(payload)))
	body = append(body, payload...)

	frame := make([]byte, reportLen)
	copy(frame[1:], body)
	frame[1+len(body)] = xorSum(body)
	return frame, nil
}

// decodeFrame: byte 0 is the report id, body starts at index 1. A payload
// whith first byte of 0 = success.
func decodeFrame(raw []byte, n int) ([]byte, error) {
	if n < 4 || n > len(raw) {
		return nil, fmt.Errorf("short response (%d bytes)", n)
	}
	resp := raw[1:n]
	length := int(resp[1])
	if 2+length >= len(resp) {
		return nil, fmt.Errorf("response length %d exceeds frame", length)
	}
	if xorSum(resp[:2+length]) != resp[2+length] {
		return nil, errors.New("response checksum mismatch")
	}
	return resp[2 : 2+length], nil
}

func (k *Keypad) command(opcode byte, payload []byte) ([]byte, error) {
	frame, err := encodeFrame(opcode, payload)
	if err != nil {
		return nil, err
	}

	k.dev.flushQueue()
	if _, err := k.dev.io(true, frame, ioTimeout); err != nil {
		return nil, fmt.Errorf("write opcode 0x%02X: %w", opcode, err)
	}

	raw := make([]byte, reportLen)
	n, err := k.dev.io(false, raw, ioTimeout)
	if err != nil {
		return nil, fmt.Errorf("read after opcode 0x%02X: %w", opcode, err)
	}
	return decodeFrame(raw, n)
}

func parseKeyChar(ch string) (byte, error) {
	if len(ch) != 1 {
		return 0, fmt.Errorf("value must be exactly one character, got %q", ch)
	}
	c := ch[0]
	switch {
	case c >= 'a' && c <= 'z':
		return c - 'a' + 'A', nil
	case c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == ' ':
		return c, nil
	case c == '\t':
		return 0x09, nil
	case c == '\n' || c == '\r':
		return 0x0D, nil
	case c == 0x08:
		return 0x08, nil
	case c == 0x1B:
		return 0x1B, nil
	}
	return 0, fmt.Errorf("unsupported character %q: use a letter, digit, space, Tab, Enter, "+
		"Backspace or Esc, or give an explicit code with --vk <hex>", ch)
}

func keyRecordForChar(ch string) (KeyRecord, error) {
	vk, err := parseKeyChar(ch)
	if err != nil {
		return KeyRecord{}, err
	}
	return KeyRecord{SubType: 0xFF, Name: strings.ToUpper(ch), Data: []byte{vk}}, nil
}

// keyRecordForVK stores an explicit code. Empty display name is acceptable.
func keyRecordForVK(vk byte) (KeyRecord, error) {
	if vk == 0 {
		return KeyRecord{}, fmt.Errorf("VK code 0x00 means \"no key\" and cannot be stored")
	}
	return KeyRecord{SubType: 0xFF, Data: []byte{vk}}, nil
}

func emptyRecord() KeyRecord {
	return KeyRecord{SubType: 0xFF, Data: []byte{0}}
}

func describeData(data []byte) string {
	if len(data) != 1 {
		return ""
	}
	switch v := data[0]; {
	case v == 0:
		return "=<none>"
	case v == 0x08:
		return "=<Backspace>"
	case v == 0x09:
		return "=<Tab>"
	case v == 0x0d:
		return "=<Enter>"
	case v == 0x1b:
		return "=<Esc>"
	case v >= 0x20 && v <= 0x7e:
		return "=" + strconv.Quote(string(rune(v)))
	}
	return fmt.Sprintf("=0x%02X", data[0])
}

// Device sets key type, sub type and trigger itself.
func recordMatches(want, got KeyRecord) bool {
	return want.Name == got.Name && bytes.Equal(want.Data, got.Data)
}

type Info struct {
	Type, SubType        byte
	Firmware, Bootloader []byte
	LayerID, LayerCount  byte
	MaxLayerCount        byte
}

func (k *Keypad) GetInfo() (Info, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.getInfo()
}

func (k *Keypad) getInfo() (Info, error) {
	d, err := k.command(opGetInfo, nil)
	if err != nil {
		return Info{}, err
	}
	if len(d) < 12 || d[0] != 0 {
		return Info{}, fmt.Errorf("unexpected GET_INFO response: %v", d)
	}
	return Info{
		Type: d[1], SubType: d[2],
		Firmware:   append([]byte(nil), d[3:6]...),
		Bootloader: append([]byte(nil), d[6:9]...),
		LayerID:    d[9], LayerCount: d[10], MaxLayerCount: d[11],
	}, nil
}

func (k *Keypad) getLayerName(layer byte) (string, error) {
	d, err := k.command(opGetLayerName, []byte{layer})
	if err != nil {
		return "", err
	}
	if len(d) < 1 || d[0] != 0 {
		return "", fmt.Errorf("unexpected GET_LAYER_NAME response: %v", d)
	}
	return decodeUTF16BE(d[1:]), nil
}

// ApplyLayer reads the layer, applies overrides, writes it back and verifies the
// result under one lock.
func (k *Keypad) ApplyLayer(layer byte, overrides map[int]KeyRecord) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	recs := make([]KeyRecord, keyCount)
	for i := 0; i < keyCount; i++ {
		cur, err := k.getKey(layer, byte(i))
		if err != nil {
			return fmt.Errorf("reading key %d: %w", i, err)
		}
		recs[i] = sanitize(cur)
		if rec, ok := overrides[i]; ok {
			recs[i] = rec
		}
	}
	if err := k.flashLayer(layer, recs); err != nil {
		return err
	}

	var bad []error
	for i := range recs {
		got, err := k.getKey(layer, byte(i))
		if err != nil {
			return fmt.Errorf("verify key %d: %w", i, err)
		}
		if !recordMatches(recs[i], got) {
			bad = append(bad, fmt.Errorf("key %d: wrote %q %v, device reports %q %v",
				i, recs[i].Name, recs[i].Data, got.Name, got.Data))
		}
	}
	if len(bad) > 0 {
		return errors.Join(bad...)
	}
	return nil
}

// sanitize maps an SRAM initialized block (data 0xFF, name U+FFFF) to the empty record.
func sanitize(rec KeyRecord) KeyRecord {
	if rec.Name == "\uFFFF" {
		rec.Name = ""
	}
	for _, b := range rec.Data {
		if b != 0xFF {
			return rec
		}
	}
	if len(rec.Data) > 0 {
		rec.Data = []byte{0}
	}
	return rec
}

type KeyRecord struct {
	KeyType byte
	SubType byte
	Trigger byte
	Name    string
	Data    []byte
}

func (k *Keypad) GetKey(layer, key byte) (KeyRecord, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.getKey(layer, key)
}

func (k *Keypad) getKey(layer, key byte) (KeyRecord, error) {
	h, err := k.command(opGetKey, []byte{0xFF, layer, key})
	if err != nil {
		return KeyRecord{}, err
	}
	if len(h) < 5 || h[0] != 0 {
		return KeyRecord{}, fmt.Errorf("unexpected GET_KEY response: %v", h)
	}
	rec := KeyRecord{KeyType: h[2], SubType: h[3], Trigger: h[4], Name: decodeUTF16BE(h[5:])}

	seq := -1
	for i := 0; i < 64; i++ {
		if seq >= 15 {
			seq = 0
		} else {
			seq++
		}
		c, err := k.command(opGetKey, []byte{byte(seq), layer, key})
		if err != nil {
			return rec, err
		}
		if len(c) < 2 {
			return rec, fmt.Errorf("malformed GET_KEY chunk: %v", c)
		}
		if c[1] == 0xFE {
			break
		}
		rec.Data = append(rec.Data, c[2:]...)
	}
	return rec, nil
}

// abortRecord closes an open record with the 0xFE sentinel so the device does not
// treat later frames as record data.
func (k *Keypad) abortRecord() {
	_, _ = k.command(opSetKey, []byte{0xFE})
}

// setKey writes a header, the data in chunks, then commit. The layer header must
// already have been sent in the same session.
func (k *Keypad) setKey(layer, key byte, rec KeyRecord) error {
	name := encodeUTF16BE(rec.Name)
	if 6+len(name) > maxPayload {
		return fmt.Errorf("key name %q is too long (%d bytes encoded, max %d)",
			rec.Name, len(name), maxPayload-6)
	}
	header := append([]byte{0xFF, layer, key, rec.KeyType, rec.SubType, rec.Trigger}, name...)
	d, err := k.command(opSetKey, header)
	if err != nil {
		return err
	}
	if d[0] != 0 {
		k.abortRecord()
		return fmt.Errorf("SET_KEY header rejected: status %d", d[0])
	}

	seq := -1
	for off := 0; off < len(rec.Data); off += chunkSize {
		end := off + chunkSize
		if end > len(rec.Data) {
			end = len(rec.Data)
		}
		if seq >= 15 {
			seq = 0
		} else {
			seq++
		}
		chunk := append([]byte{byte(seq)}, rec.Data[off:end]...)
		d, err := k.command(opSetKey, chunk)
		if err != nil {
			return err
		}
		if d[0] != 0 {
			k.abortRecord()
			return fmt.Errorf("SET_KEY chunk rejected at offset %d: status %d", off, d[0])
		}
	}

	d, err = k.command(opSetKey, []byte{0xFE})
	if err != nil {
		return err
	}
	if d[0] != 0 {
		return fmt.Errorf("SET_KEY commit rejected: status %d", d[0])
	}
	return nil
}

func (k *Keypad) setLayerHeader(layer, lastLayer byte, name string) error {
	payload := append([]byte{layer, lastLayer}, encodeUTF16BE(name)...)
	d, err := k.command(opLayerHeader, payload)
	if err != nil {
		return err
	}
	if d[0] != 0 {
		return fmt.Errorf("layer header rejected: status %d", d[0])
	}
	return nil
}

func (k *Keypad) flashLayer(layer byte, recs []KeyRecord) error {
	info, err := k.getInfo()
	if err != nil {
		return err
	}
	name, err := k.getLayerName(layer)
	if err != nil || name == "" {
		name = "Layer1"
	}
	last := byte(0)
	if info.LayerCount > 0 {
		last = info.LayerCount - 1
	}
	if err := k.setLayerHeader(layer, last, name); err != nil {
		return err
	}
	for i := range recs {
		time.Sleep(flushDelay)
		if err := k.setKey(layer, byte(i), recs[i]); err != nil {
			return fmt.Errorf("key %d: %w", i, err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if err := k.commitLayer(layer, last); err != nil {
		return err
	}
	time.Sleep(50 * time.Millisecond)
	_, _ = k.getInfo()
	return nil
}

// commitLayer sends 0x66 with the layer and last-layer index for NVRAM write.
func (k *Keypad) commitLayer(layer, last byte) error {
	d, err := k.command(opCommitLayer, []byte{layer, last})
	if err != nil {
		return err
	}
	if d[0] != 0 {
		return fmt.Errorf("layer commit rejected: status %d", d[0])
	}
	return nil
}

func (k *Keypad) readEvent(buf []byte) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.dev.io(false, buf, 300*time.Millisecond)
}

func encodeUTF16BE(s string) []byte {
	out := make([]byte, 0, 2*len(s))
	for _, r := range s {
		out = append(out, byte(r>>8), byte(r))
	}
	return out
}

func decodeUTF16BE(b []byte) string {
	var sb strings.Builder
	for i := 0; i+1 < len(b); i += 2 {
		sb.WriteRune(rune(uint16(b[i])<<8 | uint16(b[i+1])))
	}
	return sb.String()
}