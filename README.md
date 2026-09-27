# usbformat

A small GTK4 GUI for formatting a USB drive. Pick a drive, pick a filesystem,
confirm, and it asks for your password and formats it. It then mounts the result
so it is usable straight away.

Only removable drives (`rm=1`) and USB-bus disks (`tran=usb`) are ever listed.
Formatting a built-in disk is unrecoverable and "removable" is the only portable
signal that a disk is a stick.

## Build and run

Needs GTK4 and its development headers, because the bindings are cgo.

```sh
go build -o usbformat .
./usbformat
```

```sh
./usbformat --self-test   # no display or root needed
```

## Runtime tools

| needed for | package |
|---|---|
| listing drives | `util-linux` (`lsblk`) |
| clearing the old partition table | `parted` (`parted`, `partprobe`) |
| exfat | `exfatprogs` |
| fat32 | `dosfstools` |
| ext4 | `e2fsprogs` |
| opening the drive afterwards | `udisks2` (`udisksctl`) |
| waiting for the new partition node | `systemd` (`udevadm`) |
| desktop notification | `libnotify` (`notify-send`) |
| asking for the password | `pkexec` |

A missing `mkfs` is reported with the install command for your package manager
rather than failing at the root prompt.

## How it works

The GUI never runs as root. It re-executes itself through `pkexec` for the
format only, then mounts the result as you and posts a notification.

```
./usbformat                      GUI, runs as you
  └─ pkexec ./usbformat --format sdc fat32     the only root part
       ├─ umount        the drive and any partitions on it
       ├─ parted mklabel msdos                 drops the stale GPT/MBR
       ├─ parted mkpart primary 1MiB 100%     one partition, the whole device
       ├─ partprobe && udevadm settle         wait for /dev/sdc1 to exist
       └─ mkfs.vfat -F 32 /dev/sdc1
  ├─ udisksctl mount -b /dev/sdc1              back as you
  └─ notify-send                               success only
```

`mklabel` matters: without it a leftover GPT leaves phantom partitions that
survive the erase. `mkpart` matters just as much — a filesystem written to the
raw device mounts fine with `udisks2`, but Windows and macOS only enumerate
partitions, so a whole-device filesystem is invisible to both. Without it the
`exfat - Windows, macOS, Linux` option is a lie.

## Safety

- The confirm dialog puts keyboard focus on **Cancel**, so Enter cannot erase a
  drive by reflex. Erasing needs a deliberate click on **Erase**.
- The erase button stays insensitive until a drive is selected, and controls
  disable while a format is running.
- The password prompt is the polkit agent's, not a terminal.

## Notes

`fat32` is offered as "universal, max 32 GB", which is a soft warning rather
than a hard limit — `dosfstools` will happily make a FAT32 filesystem on a
larger drive. The kernel will not mount exfat unless the `exfat` module is
available, so on a machine that has not rebooted since a kernel upgrade the
erase succeeds and the mount does not.
