package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const usageTemplate = `{prog} - Vaydeer 9-key keypad programmer

usage: {prog} <command> [args]

  info                        device type and firmware version
  identity                    product / manufacturer / serial strings
  dump                        print all nine key records
  set <index> <char|--vk hex> map one key to a validated key character or VK code
  clear <index>               make a key do nothing
  factory                     write the factory 1..9 mapping
  listen [--seconds N]        print raw key events from the event channel

Key indices are row-major from the top-left:

    index:  0 1 2      default key:  7 8 9
            3 4 5                    4 5 6
            6 7 8                    1 2 3

`

func usage() string {
	return strings.ReplaceAll(usageTemplate, "{prog}", programName())
}

func programName() string {
	return filepath.Base(os.Args[0])
}

var factoryRecords = []KeyRecord{
	{Name: "7", Data: []byte{55}}, {Name: "8", Data: []byte{56}}, {Name: "9", Data: []byte{57}},
	{Name: "4", Data: []byte{52}}, {Name: "5", Data: []byte{53}}, {Name: "6", Data: []byte{54}},
	{Name: "1", Data: []byte{49}}, {Name: "2", Data: []byte{50}}, {Name: "3", Data: []byte{51}},
}

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage())
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	switch cmd {
	case "info":
		runInfo()
	case "identity":
		runIdentity()
	case "dump":
		runDump()
	case "set":
		runSet(args)
	case "clear":
		runClear(args)
	case "factory":
		runFactory()
	case "listen":
		runListen(args)
	case "-h", "--help", "help":
		fmt.Print(usage())
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		fmt.Print(usage())
		os.Exit(2)
	}
}

func mustOpen() *Keypad {
	k, err := Open()
	if err != nil {
		fatal(err)
	}
	return k
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

// activeLayer: keypad only exposes a single layer, but must be selected for read/writes.
func activeLayer(k *Keypad) byte {
	info, err := k.GetInfo()
	if err != nil {
		fatal(err)
	}
	return info.LayerID
}

func runInfo() {
	k := mustOpen()
	defer k.Close()
	info, err := k.GetInfo()
	if err != nil {
		fatal(err)
	}
	fmt.Printf("type          : %d\n", info.Type)
	fmt.Printf("sub type      : %d (keys)\n", info.SubType)
	fmt.Printf("firmware      : %d.%d.%d\n", info.Firmware[0], info.Firmware[1], info.Firmware[2])
	fmt.Printf("bootloader    : %d.%d.%d\n", info.Bootloader[0], info.Bootloader[1], info.Bootloader[2])
}

func runIdentity() {
	k := mustOpen()
	defer k.Close()
	product, manufacturer, serial, err := k.Identity()
	if err != nil {
		fatal(err)
	}
	fmt.Printf("product      : %s\n", product)
	fmt.Printf("manufacturer : %s\n", manufacturer)
	if serial != "" {
		fmt.Printf("serial       : %s\n", serial)
	}
	fmt.Printf("required     : contains %q / %q (verified)\n", productFrag, manufacturerFrag)
}

func runDump() {
	k := mustOpen()
	defer k.Close()
	layer := activeLayer(k)
	for i := 0; i < keyCount; i++ {
		rec, err := k.GetKey(layer, byte(i))
		if err != nil {
			fmt.Printf("key[%d]: error: %v\n", i, err)
			continue
		}
		fmt.Printf("key[%d] type=%d sub=%d trig=%d name=%q data=%v%s\n",
			i, rec.KeyType, rec.SubType, rec.Trigger, rec.Name, rec.Data, describeData(rec.Data))
	}
}

func runSet(args []string) {
	vkCode := -1
	filtered := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--vk" && i+1 < len(args) {
			v, err := strconv.ParseUint(strings.TrimPrefix(args[i+1], "0x"), 16, 8)
			if err != nil || v == 0 {
				fatal(fmt.Errorf("--vk needs a hex code in 0x01..0xff"))
			}
			vkCode = int(v)
			i++
			continue
		}
		filtered = append(filtered, args[i])
	}

	rest := filtered
	if len(rest) < 1 || (len(rest) < 2 && vkCode < 0) {
		fatal(fmt.Errorf("usage: %s set <index> <char|--vk hex>", programName()))
	}
	idx, err := strconv.Atoi(rest[0])
	if err != nil || idx < 0 || idx >= keyCount {
		fatal(fmt.Errorf("index must be 0..%d", keyCount-1))
	}

	var rec KeyRecord
	if vkCode >= 0 {
		rec, err = keyRecordForVK(byte(vkCode))
	} else {
		rec, err = keyRecordForChar(rest[1])
	}
	if err != nil {
		fatal(err)
	}

	k := mustOpen()
	defer k.Close()
	layer := activeLayer(k)
	if err := k.ApplyLayer(layer, map[int]KeyRecord{idx: rec}); err != nil {
		fatal(err)
	}
	fmt.Println("done")
}

func runClear(args []string) {
	if len(args) != 1 {
		fatal(fmt.Errorf("usage: %s clear <index>", programName()))
	}
	idx, err := strconv.Atoi(args[0])
	if err != nil || idx < 0 || idx >= keyCount {
		fatal(fmt.Errorf("index must be 0..%d", keyCount-1))
	}

	k := mustOpen()
	defer k.Close()
	if err := k.ApplyLayer(activeLayer(k), map[int]KeyRecord{idx: emptyRecord()}); err != nil {
		fatal(err)
	}
	fmt.Println("done")
}

func runFactory() {
	k := mustOpen()
	defer k.Close()
	layer := activeLayer(k)

	overrides := make(map[int]KeyRecord, keyCount)
	for i, rec := range factoryRecords {
		rec.SubType = 0xFF
		overrides[i] = rec
	}
	if err := k.ApplyLayer(layer, overrides); err != nil {
		fatal(err)
	}
	fmt.Println("done")
}

var keyActions = map[byte]string{0: "press", 2: "release"}

func runListen(args []string) {
	seconds := 20
	for i := 0; i < len(args); i++ {
		if args[i] == "--seconds" && i+1 < len(args) {
			if v, err := strconv.Atoi(args[i+1]); err == nil && v > 0 {
				seconds = v
			}
		}
	}
	k, err := OpenEventChannel()
	if err != nil {
		fatal(err)
	}
	defer k.Close()

	fmt.Printf("listening on the event channel for %d s (Ctrl+C to stop)\n", seconds)
	fmt.Println("index 0..8 -> labels 7 8 9 / 4 5 6 / 1 2 3; action 0=press 2=release")

	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	buf := make([]byte, reportLen)
	for time.Now().Before(deadline) {
		n, err := k.readEvent(buf)
		if err == errTimeout {
			continue
		}
		if err != nil {
			fatal(err)
		}
		// [reportID][0xFB][len=3][layer][key][action][xor]
		if n < 7 || buf[1] != opKeyEvent || buf[2] != 3 {
			continue
		}
		idx, action := buf[4], buf[5]
		what := keyActions[action]
		if what == "" {
			what = fmt.Sprintf("action %d", action)
		}
		fmt.Printf("  key=%d %s\n", idx, what)
	}
}