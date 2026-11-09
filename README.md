# Vaydeer 9-Key Programmer
## A simple, 2 MB command-line key programmer for Windows, MacOS, Linux

`Vaydeer9` is a minimal, compiled Go program that changes what the nine keys on the
Vaydeer keypad do. You tell it which key to change and which character you want, and it
writes that into the keypad and verifies the change.  Nothing to install.

> [!NOTE]
> Linux code is currently untested.

## What you need

* The keypad plugged into the computer.
* The `Vaydeer9` executable.

Open a terminal window to the folder with the Vaydeer9 executable, then proceed as shown below.  _On Linux only, additional steps (below) are necessary to grant access for non-root execution._

## Key layout

The program refers to keys by a **number from 0 to 8**, counted left to right, top to
bottom, starting at the top-left corner.

That number is the **position** of the key, not the number pad digit:

```
   numberpad key orientation       key index for commands

      +-----+-----+-----+           +-----+-----+-----+
      |  7  |  8  |  9  |           |  0  |  1  |  2  |
      +-----+-----+-----+           +-----+-----+-----+
      |  4  |  5  |  6  |           |  3  |  4  |  5  |
      +-----+-----+-----+           +-----+-----+-----+
      |  1  |  2  |  3  |           |  6  |  7  |  8  |
      +-----+-----+-----+           +-----+-----+-----+
```

## View the current key program

```
Vaydeer9.exe dump
```

This prints one line for each key, in number order. It looks like this:

```
key[0] type=0 sub=255 trig=0 name="7" data=[55]="7"
```

* `key[0]` - which key this line is about (position 0, the top-left key).
* `name="7"` - the label stored for that key.
* `data=[55]="7"` - the character the key actually types. The part in brackets is the code
  the keypad stores (55 is the code for the digit 7); the part in quotes is that code
  spelled out, so you can normally just read the end of the line.

A key that has been made to do nothing shows `data=[0]=<none>`. Named keys show their
name, for example `data=[13]=<Enter>`, and anything the program does not recognize is
shown as a number, for example `data=[1]=0x01`.

## Change a key assignment

```
Vaydeer9.exe set <key number> <character>
```

For example:

| Command | Result |
|---|---|
| `Vaydeer9.exe set 0 A` | the top-left key now types `a` |
| `Vaydeer9.exe set 8 9` | the bottom-right key now types `9` |
| `Vaydeer9.exe set 4 " "` | the middle key now types a space |
| `Vaydeer9.exe set 6 z` | the bottom-left key now types `z` |

Notes on the examples:

* Put the character in quotes only if it is a space: `" "`.
* Letters are stored as capital letters but **type in lower case**, because the keypad
  behaves like pressing the letter without holding Shift. `set 0 A` gives you `a`.
* One character per key, and the character must be a letter, a digit, or a space.

When the write is confirmed, the program prints `done`.

## Change multiple keys

Run one command per key:

```
Vaydeer9.exe set 6 A
Vaydeer9.exe set 7 B
Vaydeer9.exe set 8 C
```

That makes the bottom row type `abc` instead of `123`. Changing the top-left key after
that does not disturb the others.

## Mute a key

```
Vaydeer9.exe clear 4
```

Key 4 (the middle key) now types nothing at all. Useful for keys you keep pressing by
mistake.

## Write the factory key assignments

```
Vaydeer9.exe factory
```

Restores the original 1 to 9 number pad key assignments.

## Check for a supported keypad

```
Vaydeer9.exe identity
```

Prints the device name, the maker, and the serial number. The program will refuse commands
if a supported device is not detected.

## Hardware function test

```
Vaydeer9.exe listen --seconds 20
```

Press keys while it runs and it prints which key was pressed and whether it went down or
came up. Handy for finding the number of a key.

## Troubleshooting

| Message | What it means |
|---|---|
| `error: keypad not found` | The keypad is not plugged in, or another program has it open. |
| `error: not a Vaydeer 9-key keypad` | An unsupported keypad is found. |
| `error: index must be 0..8` | The key number must be 0 to 8. See the grids above. |
| `error: unsupported character` | Use a letter, a digit, or a space. |
| `error: usage: ...` | A part of the command is missing; compare with the examples above. |

On success, a command outputs `done`.

---

## Building the source code yourself

`go build -trimpath -ldflags '-s -w' -o Vaydeer9.exe .`

On Linux the build is pure Go, so it cross-compiles from any host:

```bash
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o Vaydeer9-linux-amd64 .
GOOS=linux GOARCH=arm64 go build -trimpath -ldflags '-s -w' -o Vaydeer9-linux-arm64 .
```

macOS needs cgo, because IOKit is a C framework, so the build has to run on a Mac with
Xcode Command Line Tools installed (`xcode-select --install`):

```bash
./build-mac.sh
```

## Linux: allowing non-root access to the keypad (udev rule)

On Linux the keypad's interfaces show up as `/dev/hidrawN` nodes, which are owned by root.
Unless you grant access, the Vaydeer9 binary needs to be run as root. One udev rule covers
all of the keypad's interfaces.

**1. Confirm the kernel sees the keypad**

```bash
lsusb | grep -i 0483:5752
```

The numbers are the vendor and product ids: `0483` and `5752`.

**2. Create the rule**

```bash
sudo tee /etc/udev/rules.d/70-vaydeer-keypad.rules >/dev/null <<'EOF'
# Vaydeer 9-key keypad: let the logged-in user open the vendor HID interfaces.
SUBSYSTEM=="hidraw", ATTRS{idVendor}=="0483", ATTRS{idProduct}=="5752", TAG+="uaccess"
EOF
```

The leading number sets the order; `70-` runs after the built-in HID rules. `TAG+="uaccess"`
grants access to whoever is logged in at the console. On a system without `uaccess`, use
`MODE="0666"` (any user) or `GROUP="plugdev", MODE="0660"` (a group you belong to).

**3. Load the rule**

```bash
sudo udevadm control --reload-rules
sudo udevadm trigger --subsystem-match=hidraw
```

If the tool still cannot open the device, unplug the keypad and plug it back in.

**4. Check the permissions changed**

```bash
ls -l /dev/hidraw*
```

Each keypad node should now show a `+` (an ACL was applied) or the mode you asked for:

```
crw-rw----+ 1 root root 241, 3 ... /dev/hidraw3
```

**5. Testing**

```bash
./Vaydeer9-linux-amd64 info
```

That should print the type, sub type and firmware version. A `permission denied` error means
the rule did not match - see below.

### If the rule does not match

`idVendor` and `idProduct` live on the parent USB device, not on the hidraw node itself, so
the rule must use `ATTRS{...}` (plural). `ATTR{...}` silently matches nothing. To see what a
node actually offers:

```bash
udevadm info --attribute-walk --name=/dev/hidraw0 | grep -E 'idVendor|idProduct|KERNEL'
```

After editing the rule, apply it again and re-check:

```bash
sudo udevadm control --reload-rules
sudo udevadm trigger --subsystem-match=hidraw
```

Running the tool as root also works and is a quick way to tell a permission problem apart
from a protocol problem.

## Advanced Guidance

A key's value is a **VK code, not a character**, so `set` accepts only characters whose
byte is the VK code the user means:

* `A`-`Z` (lower case is folded to upper), `0`-`9`, space
* Tab, Enter (also CR/LF), Backspace, Esc

Everything else is rejected, because storing the raw byte would produce a
different key.  Example: `.` is `VK_DELETE`, `;`, `a` is `VK_NUMPAD1` and `z`
is `VK_F11`. For those, pass an explicit code:

```
Vaydeer9.exe set 3 --vk 0xBA     ; VK_OEM_1, the ';' key
```