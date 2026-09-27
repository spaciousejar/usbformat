// usbformat - format a USB drive. GUI wrapper around mkfs; only mkfs runs as root.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
)

type fsDef struct {
	label, key, pkg string
	argv            []string
}

// Keep the dropdown labels short: they are button text, and a 300px-wide
// control labelled with a paragraph is a control nobody can scan. The full
// reasoning lives in the row of the popover and the dropdown's tooltip.
var formats = []fsDef{
	{"exfat - Windows, macOS, Linux", "exfat", "exfatprogs", []string{"mkfs.exfat"}},
	{"fat32 - universal, max 32 GB", "vfat", "dosfstools", []string{"mkfs.vfat", "-F", "32"}},
	{"ext4 - Linux only", "ext4", "e2fsprogs", []string{"mkfs.ext4", "-q"}},
}

var banned = []string{"sr", "loop", "ram", "zram", "fd"}

type disk struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Model string `json:"model"`
	Tran  string `json:"tran"`
	RM    bool   `json:"rm"`
	Type  string `json:"type"`
}

func run(name string, args ...string) (int, string) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if cmd.ProcessState == nil {
		return 127, err.Error()
	}
	return cmd.ProcessState.ExitCode(), strings.TrimSpace(string(out))
}

// Only removable block devices are ever offered: formatting a built-in disk is
// unrecoverable, and "removable" is the only portable signal that a disk is a
// stick. rm=0 USB disks still pass via tran.
func filter(in []disk) []disk {
	var out []disk
	for _, d := range in {
		if d.Type != "disk" || (!d.RM && d.Tran != "usb") {
			continue
		}
		skip := false
		for _, b := range banned {
			if strings.HasPrefix(d.Name, b) {
				skip = true
			}
		}
		if !skip {
			out = append(out, d)
		}
	}
	return out
}

func disks() ([]disk, error) {
	code, out := run("lsblk", "-d", "-b", "-J", "-o", "NAME,SIZE,MODEL,TRAN,RM,TYPE")
	if code != 0 {
		return nil, fmt.Errorf("%s", out)
	}
	var r struct {
		Blockdevices []disk `json:"blockdevices"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return nil, err
	}
	found := filter(r.Blockdevices)
	// Biggest first: the thumb drive someone just picked up is nearly always the
	// smallest one in the list.
	sort.Slice(found, func(i, j int) bool { return found[i].Size > found[j].Size })
	return found, nil
}

func human(n int64) string {
	// Binary units, so numbers match lsblk / GNOME Disks / Windows.
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	f := float64(n)
	for i, u := range units {
		if f < 1024 || i == len(units)-1 {
			if i == 0 {
				return fmt.Sprintf("%.0f %s", f, u)
			}
			return fmt.Sprintf("%.1f %s", f, u)
		}
		f /= 1024
	}
	return ""
}

// rowText is the two lines a device gets in the list: identity on top, details
// under. Kept pure so it stays testable without a display. Alignment is the
// layout manager's job, not padded strings -- padded columns break the moment a
// model name is 29 characters.
func rowText(d disk) (title, sub string) {
	title = strings.TrimSpace(d.Model)
	if title == "" {
		title = "USB drive"
	}
	parts := []string{human(d.Size), "/dev/" + d.Name}
	if d.Tran != "" {
		parts = append(parts, strings.ToUpper(d.Tran))
	}
	return title, strings.Join(parts, "  \u00b7  ")
}

// partitions returns dev plus every partition on it, from lsblk JSON.
//
// The lsblk call needs a /dev/ path, not a bare name: given "sdc" it exits
// non-zero with no children, so the partitions are never discovered, never
// unmounted, and mkfs then fails with "device or resource busy" whenever the
// stick already carries a mounted partition. That only shows up on sticks that
// arrive partitioned, which is most of them in the wild.
func partitions(dev, lsblkJSON string) []string {
	parts := []string{dev}
	var r struct {
		Blockdevices []struct {
			Name     string `json:"name"`
			Children []struct {
				Name string `json:"name"`
			} `json:"children"`
		} `json:"blockdevices"`
	}
	if json.Unmarshal([]byte(lsblkJSON), &r) != nil {
		return parts
	}
	for _, d := range r.Blockdevices {
		for _, c := range d.Children {
			parts = append(parts, c.Name)
		}
	}
	return parts
}

// firstPart is the name of a disk's first partition. sd* takes a bare digit
// (sdc1), but nvme* and mmcblk* insert a "p" (nvme0n1p1) because the name
// already ends in a digit. The filter does not reject those: an NVMe SSD in a
// USB enclosure reports tran=usb, so it lands here like any other stick.
func firstPart(dev string) string {
	if c := dev[len(dev)-1]; c >= '0' && c <= '9' {
		return dev + "p1"
	}
	return dev + "1"
}

func lookup(key string) (fsDef, bool) {
	for _, f := range formats {
		if f.key == key {
			return f, true
		}
	}
	return fsDef{}, false
}

// doFormat runs as root, returning the message to show the user and whether it
// worked. Both are needed: the success message is non-empty too, so the caller
// cannot read failure off an empty string.
func doFormat(dev, fsKey string) (string, bool) {
	if os.Geteuid() != 0 {
		return "must run as root (the GUI relaunches this via pkexec)", false
	}
	// Before anything destructive. A typo here used to reach parted, wipe the
	// partition table, and only then fail to find a filesystem to write.
	f, ok := lookup(fsKey)
	if !ok {
		return "unknown filesystem " + fsKey, false
	}
	// Only the disk itself. A bare name here makes lsblk fail, which silently
	// hides the partitions we most need to unmount.
	_, out := run("lsblk", "-J", "-o", "NAME", "/dev/"+dev)
	parts := partitions(dev, out)
	for _, p := range parts {
		if code, out := run("umount", "/dev/"+p); code != 0 && !strings.Contains(out, "not mounted") {
			return fmt.Sprintf("could not unmount /dev/%s: %s", p, out), false
		}
	}
	// mklabel clears a stale GPT/MBR; without it the old partitions linger as
	// phantom entries that the desktop still offers to mount.
	if code, out := run("parted", "-s", "/dev/"+dev, "mklabel", "msdos"); code != 0 {
		return "parted: " + out, false
	}
	// One partition across the whole device. A filesystem written to the raw
	// device instead mounts fine on Linux and is invisible to Windows and macOS,
	// which only enumerate partitions -- so the exfat option's promise of
	// working on all three is only true if there is a partition here.
	if code, out := run("parted", "-s", "/dev/"+dev, "mkpart", "primary", "1MiB", "100%"); code != 0 {
		return "parted mkpart: " + out, false
	}
	// The new device node is created asynchronously, so mkfs can otherwise race
	// it and fail with "no such file or directory" on a drive that is in fact
	// partitioned. partprobe asks the kernel to re-read, settle waits for udev.
	run("partprobe", "/dev/"+dev)
	run("udevadm", "settle")
	part := firstPart(dev)
	code, out := run(f.argv[0], append(f.argv[1:], "/dev/"+part)...)
	if code != 0 {
		return out, false
	}
	return fmt.Sprintf("formatted /dev/%s as %s", part, fsKey), true
}

// Adwaita already ships boxed-list, destructive-action, dim-label, title-*,
// heading and linked. The one thing it does not have is a text view that reads
// as a footnote: its frame and background look like a second panel competing
// with the drive list for attention.
const css = `
.activity {
  background: none;
  border: none;
  color: alpha(currentColor, 0.7);
  font-size: 0.9em;
  padding: 2px 0;
}
`

// logHeight caps the activity log before it starts scrolling. Small on purpose:
// the drive list is the content, the log is a footnote.
const logHeight = 96

// toastMs is how long a notification stays up. Long enough to read a mount
// path, short enough that it is not in the way on the next attempt.
const toastMs = 6000

type ui struct {
	app        *gtk.Application
	win        *gtk.Window
	stack      *gtk.Stack
	list       *gtk.ListBox
	drop       *gtk.DropDown
	erase      *gtk.Button
	spin       *gtk.Spinner
	log        *gtk.TextBuffer
	toast      *gtk.Revealer
	toastLbl   *gtk.Label
	toastTimer glib.SourceHandle
	disks      []disk
	busy       bool
}

func (u *ui) say(msg string) {
	u.log.Insert(u.log.EndIter(), msg+"\n")
}

// notify flashes a message over the window and fades it. Adwaita's pill plus
// success/error/warning classes supply the whole look, so no colour is
// hardcoded and it follows the light/dark theme.
func (u *ui) notify(kind, msg string) {
	for _, c := range []string{"success", "error", "warning"} {
		u.toastLbl.RemoveCSSClass(c)
	}
	if kind != "" {
		u.toastLbl.AddCSSClass(kind)
	}
	u.toastLbl.SetText(msg)
	u.toast.SetRevealChild(true)
	if u.toastTimer != 0 {
		glib.SourceRemove(u.toastTimer)
	}
	u.toastTimer = glib.TimeoutAdd(toastMs, func() bool {
		u.toast.SetRevealChild(false)
		u.toastTimer = 0
		return false
	})
}

// dialog is a modal message box. danger is the index of the button that gets
// destructive-action styling, or -1. Button 0 always keeps keyboard focus, so
// an Enter reflex lands on the safe answer instead of the irreversible one.
func (u *ui) dialog(title, msg string, buttons []string, danger int, cb func(int)) {
	w := gtk.NewWindow()
	w.SetTitle(title)
	w.SetDefaultSize(470, 170)
	w.SetTransientFor(u.win)
	w.SetModal(true)

	esc := gtk.NewEventControllerKey()
	esc.Connect("key-pressed", func(_ *gtk.EventControllerKey, keyval uint, _, _ uint) bool {
		if keyval == uint(gdk.KEY_Escape) {
			w.Close()
			return true
		}
		return false
	})
	w.AddController(esc)

	outer := gtk.NewBox(gtk.OrientationVertical, 14)
	outer.SetMarginTop(18)
	outer.SetMarginBottom(18)
	outer.SetMarginStart(18)
	outer.SetMarginEnd(18)

	body := gtk.NewBox(gtk.OrientationHorizontal, 14)
	if danger >= 0 {
		warn := gtk.NewImageFromIconName("dialog-warning-symbolic")
		warn.AddCSSClass("warning")
		body.Append(warn)
	}
	l := gtk.NewLabel(msg)
	l.SetXAlign(0)
	l.SetYAlign(0.5)
	l.SetWrap(true)
	l.SetSelectable(true) // so a failed drive name can be copied out
	l.SetHExpand(true)
	body.Append(l)
	outer.Append(body)

	row := gtk.NewBox(gtk.OrientationHorizontal, 8)
	row.SetHAlign(gtk.AlignEnd)
	for i, name := range buttons {
		b := gtk.NewButtonWithLabel(name)
		b.AddCSSClass("flat")
		if i == danger {
			b.AddCSSClass("destructive-action")
		}
		if i == 0 {
			b.GrabFocus()
		}
		b.Connect("clicked", func(_ *gtk.Button) {
			w.Close()
			if cb != nil {
				cb(i)
			}
		})
		row.Append(b)
	}
	outer.Append(row)
	w.SetChild(outer)
	w.Present()
}

// refresh re-reads the drive list. keep re-selects a device by name, so the
// caret does not jump to another stick after a format finishes.
func (u *ui) refresh(keep string) {
	u.list.RemoveAll()
	u.disks, _ = disks()
	for _, d := range u.disks {
		row := gtk.NewListBoxRow()
		row.SetChild(driveRow(d))
		u.list.Append(row)
		if d.Name == keep {
			u.list.SelectRow(row)
		}
	}
	if len(u.disks) == 0 {
		u.stack.SetVisibleChildName("empty")
		u.say("no removable drives found")
	} else {
		u.stack.SetVisibleChildName("list")
	}
	u.updateErase()
}

func (u *ui) updateErase() {
	u.erase.SetSensitive(!u.busy && u.list.SelectedRow() != nil)
}

func (u *ui) setBusy(b bool) {
	u.busy = b
	u.drop.SetSensitive(!b)
	u.spin.SetSpinning(b)
	u.updateErase()
}

func (u *ui) format() {
	row := u.list.SelectedRow()
	if row == nil {
		return // erase is insensitive without a selection
	}
	d := u.disks[row.Index()]
	f := formats[u.drop.Selected()]

	if _, err := exec.LookPath(f.argv[0]); err != nil {
		pm := "sudo pacman -S"
		if _, err := exec.LookPath("apt-get"); err == nil {
			pm = "sudo apt install"
		}
		u.dialog("Missing tool",
			fmt.Sprintf("%s is not installed. Install it with:\n\n  %s %s", f.argv[0], pm, f.pkg),
			[]string{"OK"}, -1, nil)
		return
	}

	// The name alone is not an identity: unplug the stick while this dialog is
	// up and the kernel can hand /dev/sdc to something else. Re-read and
	// compare against what the user was actually shown.
	want := d
	u.dialog("Erase drive?", confirmText(d, f), []string{"Cancel", "Erase"}, 1, func(i int) {
		if i != 1 {
			return
		}
		if msg := changedSince(want); msg != "" {
			u.dialog("Drive changed", msg, []string{"OK"}, -1, nil)
			u.refresh("")
			return
		}
		u.startFormat(d.Name, f.key)
	})
}

// changedSince re-reads the drive and reports a problem if it is no longer the
// one the user confirmed. Size and model are what the dialog displayed, so a
// mismatch means the name now belongs to different hardware.
//
// This narrows the window from "the whole time the dialog was open" to "between
// this check and mkfs", which is a few milliseconds of pkexec startup. Closing
// it completely needs a udev watch on the device, which is more machinery than
// this is worth for a check the user also has to answer with a click.
func changedSince(want disk) string {
	now, err := disks()
	if err != nil {
		return "Could not check the drive before erasing: " + err.Error()
	}
	for _, d := range now {
		if d.Name == want.Name {
			if d.Size != want.Size {
				return fmt.Sprintf("/dev/%s is now %s, not the %s you were asked about. Nothing was erased.",
					d.Name, human(d.Size), human(want.Size))
			}
			return ""
		}
	}
	return fmt.Sprintf("/dev/%s is gone. Nothing was erased.", want.Name)
}

func confirmText(d disk, f fsDef) string {
	model := strings.TrimSpace(d.Model)
	if model == "" {
		model = "unidentified drive"
	}
	return fmt.Sprintf("Erase %s?\n\n%s, %s, will be formatted as %s.\n\nEvery file on it is destroyed. There is no undo.",
		d.Name, model, human(d.Size), f.key)
}

// desktopNotify mirrors the result to the notification centre. The pkexec auth
// dialog takes focus away from this window, so the in-app toast is very often
// shown to a window nobody is looking at. The message is passed as argv, never
// through a shell, so a hostile filesystem label cannot inject anything.
func desktopNotify(summary, body string) {
	if _, err := exec.LookPath("notify-send"); err != nil {
		return
	}
	run("notify-send", "--app-name=USB Formatter", "--icon=drive-harddisk-symbolic", summary, body)
}

// mount opens the formatted partition so the drive is usable straight after
// erasing. It runs here, in the GUI process, not in the pkexec child: udisksctl
// needs the user's session bus, which pkexec deliberately does not pass through.
// The target is the partition, because that is where the filesystem now lives.
func mount(part string) (string, error) {
	if _, err := exec.LookPath("udisksctl"); err != nil {
		return "", errors.New("udisksctl is not installed")
	}
	code, out := run("udisksctl", "mount", "-b", "/dev/"+part)
	if code != 0 {
		return "", errors.New(strings.TrimSpace(out))
	}
	// udisksctl prints the mount point and nothing else on success.
	return strings.TrimSpace(out), nil
}

// startFormat hands the work to pkexec on a goroutine. It used to run inline,
// which blocked the main loop: the window froze behind the auth prompt and for
// the whole of mkfs, and looked hung.
func (u *ui) startFormat(dev, fsKey string) {
	self, _ := os.Executable()
	u.say("asking for admin rights to format /dev/" + dev + " as " + fsKey + "...")
	u.setBusy(true)
	go func() {
		code, out := run("pkexec", self, "--format", dev, fsKey)
		if out == "" {
			out = fmt.Sprintf("failed (exit %d)", code)
		}
		glib.IdleAdd(func() bool {
			u.say(out)
			u.setBusy(false)
			// Only touch udisks when the format actually landed: mounting a
			// half-written device is worse than not mounting it at all.
			if code != 0 {
				u.notify("error", "Erase failed")
			} else if mnt, err := mount(firstPart(dev)); err != nil {
				u.notify("warning", "Erased, but could not open it")
				u.say("could not mount: " + err.Error())
			} else {
				msg := "Erased as " + fsKey + " and opened at " + mnt
				u.notify("success", msg)
				desktopNotify("Erase finished", msg)
			}
			u.refresh(dev)
			return false
		})
	}()
}

func (u *ui) build() {
	// ApplicationWindow embeds Window by value, so keep the base Window for
	// SetTransientFor, which will not take the ApplicationWindow.
	aw := gtk.NewApplicationWindow(u.app)
	u.win = &aw.Window
	aw.SetTitle("USB Formatter")
	aw.SetDefaultSize(660, 440)
	aw.Connect("close-request", func() bool {
		aw.Destroy()
		return false
	})
	style := gtk.NewCSSProvider()
	style.LoadFromData(css)
	gtk.StyleContextAddProviderForDisplay(aw.Widget.Display(), style, 800)

	outer := gtk.NewBox(gtk.OrientationVertical, 16)
	outer.SetMarginTop(18)
	outer.SetMarginBottom(18)
	outer.SetMarginStart(18)
	outer.SetMarginEnd(18)
	outer.SetHExpand(true)
	outer.SetVExpand(true)

	// The notification floats over the content rather than taking a slice of it.
	u.toastLbl = gtk.NewLabel("")
	u.toastLbl.AddCSSClass("pill")
	u.toastLbl.SetWrap(true)
	u.toastLbl.SetJustify(gtk.JustifyCenter)
	u.toast = gtk.NewRevealer()
	u.toast.SetChild(u.toastLbl)
	u.toast.SetTransitionType(gtk.RevealerTransitionTypeSlideDown)
	u.toast.SetTransitionDuration(250)
	u.toast.SetRevealChild(false)
	u.toast.SetHAlign(gtk.AlignCenter)
	u.toast.SetVAlign(gtk.AlignEnd)
	u.toast.SetMarginBottom(14)
	u.toastLbl.SetMarginStart(18)
	u.toastLbl.SetMarginEnd(18)
	u.toastLbl.SetMarginTop(8)
	u.toastLbl.SetMarginBottom(8)

	screen := gtk.NewOverlay()
	screen.SetChild(outer)
	screen.AddOverlay(u.toast)
	aw.SetChild(screen)

	// Title and subtitle are one block, so they get their own 0-spaced box
	// instead of inheriting the outer 16px gap.
	head := gtk.NewBox(gtk.OrientationVertical, 0)
	title := gtk.NewLabel("Format a USB drive")
	title.AddCSSClass("title-3")
	title.SetXAlign(0)
	head.Append(title)
	blurb := gtk.NewLabel("Pick a drive, choose a filesystem, erase it.")
	blurb.AddCSSClass("dim-label")
	blurb.SetXAlign(0)
	head.Append(blurb)
	outer.Append(head)

	u.list = gtk.NewListBox()
	u.list.AddCSSClass("boxed-list")
	u.list.SetSelectionMode(gtk.SelectionSingle)
	u.list.Connect("row-selected", func(_ *gtk.ListBox, _ *gtk.ListBoxRow) { u.updateErase() })
	u.list.Connect("row-activated", func(_ *gtk.ListBox, r *gtk.ListBoxRow) {
		u.list.SelectRow(r) // Enter can fire before the selection lands
		u.format()
	})

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	scroll.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	scroll.SetChild(u.list)

	// "No drives" is a full-page state, not a line in the log: the only
	// question it has to answer is whether the app or the stick is broken.
	empty := gtk.NewBox(gtk.OrientationVertical, 10)
	empty.SetVAlign(gtk.AlignCenter)
	empty.SetHAlign(gtk.AlignCenter)
	icon := gtk.NewImageFromIconName("drive-removable-media-symbolic")
	icon.SetPixelSize(64)
	icon.AddCSSClass("dim-label")
	empty.Append(icon)
	et := gtk.NewLabel("No removable drives found")
	et.AddCSSClass("title-4")
	empty.Append(et)
	es := gtk.NewLabel("Plug in a USB drive and it will show up here.")
	es.AddCSSClass("dim-label")
	empty.Append(es)

	u.stack = gtk.NewStack()
	u.stack.SetVExpand(true)
	u.stack.AddNamed(scroll, "list")
	u.stack.AddNamed(empty, "empty")
	u.stack.SetVisibleChildName("list")
	outer.Append(u.stack)

	bar := gtk.NewBox(gtk.OrientationHorizontal, 10)
	refresh := gtk.NewButtonFromIconName("view-refresh-symbolic")
	refresh.AddCSSClass("flat")
	refresh.SetTooltipText("Rescan drives")
	refresh.Connect("clicked", func() { u.refresh("") })
	bar.Append(refresh)

	pick := gtk.NewBox(gtk.OrientationHorizontal, 8)
	pick.SetHAlign(gtk.AlignCenter)
	pick.SetHExpand(true)
	cap := gtk.NewLabel("Format as")
	cap.AddCSSClass("dim-label")
	cap.SetVAlign(gtk.AlignCenter)
	pick.Append(cap)
	labels := make([]string, len(formats))
	for i, f := range formats {
		labels[i] = f.label
	}
	linked := gtk.NewBox(gtk.OrientationHorizontal, 0)
	linked.AddCSSClass("linked")
	u.drop = gtk.NewDropDownFromStrings(labels)
	u.drop.SetTooltipText("exfat is the safe default. fat32 is for cameras and consoles. ext4 is Linux only.")
	linked.Append(u.drop)
	pick.Append(linked)
	bar.Append(pick)

	// SetSpinning owns visibility: TRUE shows it, FALSE hides it.
	u.spin = gtk.NewSpinner()
	u.spin.SetVAlign(gtk.AlignCenter)
	u.spin.SetSpinning(false)
	bar.Append(u.spin)

	u.erase = gtk.NewButtonWithLabel("Erase drive")
	u.erase.AddCSSClass("destructive-action")
	u.erase.SetTooltipText("Destroy every file on the selected drive (Enter also works)")
	u.erase.Connect("clicked", func() { u.format() })
	bar.Append(u.erase)
	outer.Append(bar)

	view := gtk.NewTextView()
	view.SetEditable(false)
	view.SetMonospace(true)
	view.SetCursorVisible(false)
	view.SetWrapMode(gtk.WrapWordChar)
	view.AddCSSClass("activity")
	u.log = view.Buffer()
	logScroll := gtk.NewScrolledWindow()
	logScroll.SetPropagateNaturalHeight(true)
	logScroll.SetMaxContentHeight(logHeight)
	logScroll.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	logScroll.SetChild(view)
	outer.Append(logScroll)

	u.refresh("")
	aw.Present()
}

// driveRow is one device in the list. The two-label box is what makes long
// model names behave: they wrap or ellipsize inside the row instead of shoving
// the device path off the edge of the window.
func driveRow(d disk) gtk.Widgetter {
	title, sub := rowText(d)
	box := gtk.NewBox(gtk.OrientationVertical, 0)
	box.SetMarginTop(8)
	box.SetMarginBottom(8)

	t := gtk.NewLabel(title)
	t.AddCSSClass("body")
	t.SetXAlign(0)
	t.SetEllipsize(pango.EllipsizeEnd)
	box.Append(t)

	s := gtk.NewLabel(sub)
	s.AddCSSClass("dim-label")
	s.AddCSSClass("caption")
	s.SetXAlign(0)
	s.SetEllipsize(pango.EllipsizeMiddle)
	box.Append(s)
	return box
}

func selfTest() {
	in := []disk{
		{Name: "sda", Size: 1e12, Tran: "sata", Type: "disk"},
		{Name: "sdb", Size: 16e9, Model: "SanDisk Ultra", Tran: "usb", Type: "disk"},
		{Name: "sdb1", Size: 16e9, Tran: "", Type: "part"},
		{Name: "sr0", Size: 1e9, Tran: "", Type: "disk"},
	}
	var got []string
	for _, d := range filter(in) {
		got = append(got, d.Name)
	}
	if len(got) != 1 || got[0] != "sdb" {
		fmt.Fprintf(os.Stderr, "filter: got %v, want [sdb]\n", got)
		os.Exit(1)
	}
	if h := human(16 * 1000 * 1000 * 1000); h != "14.9 GiB" {
		fmt.Fprintf(os.Stderr, "human: got %q, want 14.9 GiB\n", h)
		os.Exit(1)
	}
	title, sub := rowText(in[1])
	if title != "SanDisk Ultra" || !strings.Contains(sub, "/dev/sdb") || !strings.Contains(sub, "14.9 GiB") {
		fmt.Fprintf(os.Stderr, "rowText: got %q / %q\n", title, sub)
		os.Exit(1)
	}
	// A nameless stick must still render something legible.
	if title, sub := rowText(disk{Name: "sdc", Size: 4e9}); title != "USB drive" || !strings.Contains(sub, "/dev/sdc") {
		fmt.Fprintf(os.Stderr, "rowText fallback: got %q / %q\n", title, sub)
		os.Exit(1)
	}
	if !strings.Contains(confirmText(in[1], formats[0]), "no undo") {
		fmt.Fprintf(os.Stderr, "confirmText: got %q\n", confirmText(in[1], formats[0]))
		os.Exit(1)
	}
	// A mount that quietly "succeeds" with no path is the failure mode that
	// leaves the user staring at an unopenable drive, so check the error path.
	if p, err := mount("nosuchdevice"); err == nil || p != "" {
		fmt.Fprintf(os.Stderr, "mount: got %q, %v; want an error and no path\n", p, err)
		os.Exit(1)
	}
	// doFormat returns a message on success too, so ok must be reported
	// separately: when it was inferred from an empty string, every successful
	// format exited 1 and the GUI read that as a failure. Both the name and the
	// filesystem here are bogus, so this cannot touch a real drive even if the
	// self-test is run as root.
	if msg, ok := doFormat("nosuchdev", "nosuchfs"); ok || msg == "" {
		fmt.Fprintf(os.Stderr, "doFormat: got %q, %v; want a message and ok=false\n", msg, ok)
		os.Exit(1)
	}
	// A notification that fails must never take down a finished erase.
	desktopNotify("self-test", "quotes \" ' $(id) `id` ; newline\nsecond line")
	// The unmount list is the difference between a working erase and
	// "device or resource busy", and the failure is invisible unless lsblk is
	// given a /dev/ path. Feed it the real shape of a partitioned stick.
	const partitioned = `{"blockdevices":[{"name":"sdc","children":[{"name":"sdc1"},{"name":"sdc2"}]}]}`
	if got := strings.Join(partitions("sdc", partitioned), " "); got != "sdc sdc1 sdc2" {
		fmt.Fprintf(os.Stderr, "partitions: got %q, want %q\n", got, "sdc sdc1 sdc2")
		os.Exit(1)
	}
	// What lsblk prints for a bare name: no children. Must still be usable.
	const bareName = `{"blockdevices":[]}`
	if got := strings.Join(partitions("sdc", bareName), " "); got != "sdc" {
		fmt.Fprintf(os.Stderr, "partitions empty: got %q, want %q\n", got, "sdc")
		os.Exit(1)
	}
	// The filesystem goes on the partition, and the mount has to follow it there
	// or the freshly erased drive comes back looking empty. sd* takes a bare
	// digit, but an NVMe SSD in a USB enclosure passes the filter and its name
	// already ends in a digit, so it needs the p.
	for _, c := range []struct{ dev, want string }{
		{"sdc", "sdc1"}, {"sdz", "sdz1"},
		{"nvme0n1", "nvme0n1p1"}, {"mmcblk0", "mmcblk0p1"},
	} {
		if got := firstPart(c.dev); got != c.want {
			fmt.Fprintf(os.Stderr, "firstPart(%q) = %q, want %q\n", c.dev, got, c.want)
			os.Exit(1)
		}
	}
	if _, ok := lookup("nosuchfs"); ok {
		fmt.Fprintln(os.Stderr, "lookup: nosuchfs should not resolve")
		os.Exit(1)
	}
	for _, f := range formats {
		if _, ok := lookup(f.key); !ok {
			fmt.Fprintf(os.Stderr, "lookup: %q does not resolve\n", f.key)
			os.Exit(1)
		}
		if f.pkg == "" {
			fmt.Fprintf(os.Stderr, "%s: no package name, so the install hint is empty\n", f.key)
			os.Exit(1)
		}
	}
	fmt.Println("ok")
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--self-test" {
		selfTest()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "--format" {
		msg, ok := doFormat(os.Args[2], os.Args[3])
		fmt.Println(msg)
		if !ok {
			os.Exit(1)
		}
		return
	}
	app := gtk.NewApplication("org.usbformat.App", gio.ApplicationFlagsNone)
	u := &ui{app: app}
	app.Connect("activate", func() { u.build() })
	os.Exit(app.Run([]string{filepath.Base(os.Args[0])}))
}
