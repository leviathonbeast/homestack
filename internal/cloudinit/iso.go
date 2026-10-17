package cloudinit

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
)

// A NoCloud seed is a small ISO 9660 disk labelled "cidata" holding
// user-data, meta-data and network-config. Homestack writes the image itself
// (rather than shipping genisoimage), with Joliet names so Linux sees
// "user-data" and not "USER_DAT".
//
// Layout, in 2048-byte sectors:
//
//	0-15  system area (empty)
//	16    primary volume descriptor     17  Joliet volume descriptor
//	18    descriptor set terminator
//	19-22 path tables (little and big endian, for each tree)
//	23    root directory                24  Joliet root directory
//	25... file data, shared by both trees

const sector = 2048

type isoFile struct {
	name string
	data []byte
}

func seedISO(label string, files []isoFile, when time.Time) ([]byte, error) {
	when = when.UTC()
	const (
		pvdSec, svdSec, termSec      = 16, 17, 18
		lPath, mPath, jlPath, jmPath = 19, 20, 21, 22
		rootSec, jRootSec, dataSec   = 23, 24, 25
	)
	extent := make([]uint32, len(files))
	next := uint32(dataSec)
	for i, f := range files {
		extent[i] = next
		next += uint32((len(f.data) + sector - 1) / sector)
	}
	total := next

	type entry struct {
		id   []byte
		file int
	}
	var plain, joliet []entry
	seen := map[string]bool{}
	for i, f := range files {
		short := shortName(f.name)
		if seen[short] {
			return nil, fmt.Errorf("two seed files share the short name %s", short)
		}
		seen[short] = true
		plain = append(plain, entry{[]byte(short), i})
		joliet = append(joliet, entry{ucs2(f.name + ";1"), i})
	}
	for _, list := range [][]entry{plain, joliet} {
		sort.Slice(list, func(a, b int) bool { return string(list[a].id) < string(list[b].id) })
	}

	dir := func(self uint32, list []entry) ([]byte, error) {
		var b []byte
		b = append(b, dirRecord(self, sector, 2, []byte{0}, when)...)
		b = append(b, dirRecord(self, sector, 2, []byte{1}, when)...)
		for _, e := range list {
			b = append(b, dirRecord(extent[e.file], uint32(len(files[e.file].data)), 0, e.id, when)...)
		}
		if len(b) > sector {
			return nil, errors.New("too many seed files for one directory sector")
		}
		return b, nil
	}
	root, err := dir(rootSec, plain)
	if err != nil {
		return nil, err
	}
	jRoot, err := dir(jRootSec, joliet)
	if err != nil {
		return nil, err
	}

	img := make([]byte, int(total)*sector)
	at := func(s uint32) []byte { return img[int(s)*sector : int(s+1)*sector] }

	copy(at(pvdSec), volumeDescriptor(1, label, total, lPath, mPath, rootSec, when))
	copy(at(svdSec), volumeDescriptor(2, label, total, jlPath, jmPath, jRootSec, when))
	term := at(termSec)
	term[0] = 255
	copy(term[1:6], "CD001")
	term[6] = 1

	copy(at(lPath), pathTable(rootSec, binary.LittleEndian))
	copy(at(mPath), pathTable(rootSec, binary.BigEndian))
	copy(at(jlPath), pathTable(jRootSec, binary.LittleEndian))
	copy(at(jmPath), pathTable(jRootSec, binary.BigEndian))
	copy(at(rootSec), root)
	copy(at(jRootSec), jRoot)
	for i, f := range files {
		copy(img[int(extent[i])*sector:], f.data)
	}
	return img, nil
}

// shortName is the ISO 9660 level 1 (8.3, upper case) form of a name.
func shortName(name string) string {
	clean := func(s string, n int) string {
		var b strings.Builder
		for _, r := range strings.ToUpper(s) {
			if b.Len() == n {
				break
			}
			if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
			} else {
				b.WriteByte('_')
			}
		}
		return b.String()
	}
	base, ext, _ := strings.Cut(name, ".")
	return clean(base, 8) + "." + clean(ext, 3) + ";1"
}

func ucs2(s string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = binary.BigEndian.AppendUint16(b, u)
	}
	return b
}

func both16(b []byte, v uint16) {
	binary.LittleEndian.PutUint16(b, v)
	binary.BigEndian.PutUint16(b[2:], v)
}

func both32(b []byte, v uint32) {
	binary.LittleEndian.PutUint32(b, v)
	binary.BigEndian.PutUint32(b[4:], v)
}

func dirRecord(extent, size uint32, flags byte, id []byte, t time.Time) []byte {
	n := 33 + len(id)
	if len(id)%2 == 0 {
		n++ // records have an even length
	}
	r := make([]byte, n)
	r[0] = byte(n)
	both32(r[2:], extent)
	both32(r[10:], size)
	r[18] = byte(t.Year() - 1900)
	r[19] = byte(t.Month())
	r[20] = byte(t.Day())
	r[21] = byte(t.Hour())
	r[22] = byte(t.Minute())
	r[23] = byte(t.Second())
	r[25] = flags
	both16(r[28:], 1)
	r[32] = byte(len(id))
	copy(r[33:], id)
	return r
}

func pathTable(root uint32, order binary.ByteOrder) []byte {
	p := make([]byte, 10)
	p[0] = 1 // the root's identifier is one byte
	order.PutUint32(p[2:], root)
	order.PutUint16(p[6:], 1) // its own parent
	return p
}

// volumeDescriptor is the primary (type 1) or Joliet (type 2) descriptor.
func volumeDescriptor(typ byte, label string, total, lPath, mPath, root uint32, t time.Time) []byte {
	d := make([]byte, sector)
	joliet := typ == 2
	text := func(off, n int, s string) {
		field := d[off : off+n]
		if joliet {
			for i := range field {
				field[i] = " \x00"[1-i%2] // UCS-2 spaces
			}
			if n%2 == 1 {
				field[n-1] = 0
			}
			copy(field, ucs2(s))
			return
		}
		for i := range field {
			field[i] = ' '
		}
		copy(field, s)
	}
	d[0] = typ
	copy(d[1:6], "CD001")
	d[6] = 1
	text(8, 32, "LINUX")
	text(40, 32, label)
	both32(d[80:], total)
	if joliet {
		copy(d[88:], "%/E") // UCS-2 level 3
	}
	both16(d[120:], 1)
	both16(d[124:], 1)
	both16(d[128:], sector)
	both32(d[132:], 10)
	binary.LittleEndian.PutUint32(d[140:], lPath)
	binary.BigEndian.PutUint32(d[148:], mPath)
	copy(d[156:], dirRecord(root, sector, 2, []byte{0}, t))
	text(190, 128, "")
	text(318, 128, "")
	text(446, 128, "")
	text(574, 128, "HOMESTACK")
	text(702, 37, "")
	text(739, 37, "")
	text(776, 37, "")
	stamp := []byte(t.Format("20060102150405") + "00\x00")
	copy(d[813:], stamp)
	copy(d[830:], stamp)
	copy(d[847:], "0000000000000000\x00")
	copy(d[864:], "0000000000000000\x00")
	d[881] = 1
	return d
}
