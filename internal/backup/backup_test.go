package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSealOpen(t *testing.T) {
	k, err := NewKey("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("the archive")
	sealed, err := Seal(k, plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, plain) {
		t.Fatal("not encrypted")
	}
	got, err := Open(sealed, "correct horse battery")
	if err != nil || string(got) != "the archive" {
		t.Fatalf("open: %q %v", got, err)
	}
	// Two backups with the same key differ (a fresh nonce each time).
	if again, _ := Seal(k, plain); bytes.Equal(again, sealed) {
		t.Error("nonce reused")
	}

	if _, err := Open(sealed, "wrong horse battery"); !errors.Is(err, ErrPassphrase) {
		t.Errorf("wrong passphrase: %v", err)
	}
	for i, where := range []int{len(magic) + 3, len(magic) + 10, headerLen + 2, len(sealed) - 1} { // iterations, salt, body, tag
		bad := bytes.Clone(sealed)
		bad[where] ^= 1
		if _, err := Open(bad, "correct horse battery"); err == nil {
			t.Errorf("change %d (byte %d) not noticed", i, where)
		}
	}
	if _, err := Open([]byte("PK\x03\x04 a zip file"), "x"); !errors.Is(err, ErrNotBackup) {
		t.Errorf("not a backup: %v", err)
	}
	newer := bytes.Clone(sealed)
	newer[len(magic)] = 2
	if _, err := Open(newer, "correct horse battery"); err == nil || !strings.Contains(err.Error(), "newer Homestack") {
		t.Errorf("newer format: %v", err)
	}
	if err := CheckPassphrase("short"); err == nil {
		t.Error("short passphrase accepted")
	}
	if _, err := Seal(Key{}, plain); err == nil {
		t.Error("sealed without a key")
	}
}

func TestPackUnpack(t *testing.T) {
	m := Manifest{Format: 1, Created: time.Now().Truncate(time.Second), Host: "lucy", Version: "0.18.0",
		Parts: []Part{{ID: "storage", Files: []string{"etc/snapraid.conf"}}}}
	data, err := Pack(m, []Entry{{Name: "etc/snapraid.conf", Mode: 0o644, Data: []byte("parity /mnt/parity1/snapraid.parity\n")}})
	if err != nil {
		t.Fatal(err)
	}
	got, files, err := Unpack(data)
	if err != nil || got.Host != "lucy" || len(got.Parts) != 1 || string(files["etc/snapraid.conf"].Data) != "parity /mnt/parity1/snapraid.parity\n" || files["etc/snapraid.conf"].Mode != 0o644 {
		t.Fatalf("unpack: %+v %v %v", got, files, err)
	}
	if _, ok := files[ManifestName]; ok {
		t.Error("manifest left among the files")
	}
	for _, bad := range []string{"/etc/passwd", "../etc/passwd", "etc/../../x", "etc//x", `etc\x`, ""} {
		if _, err := Pack(m, []Entry{{Name: bad}}); err == nil {
			t.Errorf("packed %q", bad)
		}
	}
}

// An archive made by something else can't smuggle in paths or links.
func TestUnpackRefusesOddEntries(t *testing.T) {
	mk := func(hdrs ...*tar.Header) []byte {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		tw.WriteHeader(&tar.Header{Name: ManifestName, Mode: 0o600, Size: 12, Typeflag: tar.TypeReg})
		tw.Write([]byte(`{"format":1}`))
		for _, h := range hdrs {
			tw.WriteHeader(h)
			if h.Size > 0 {
				tw.Write(make([]byte, h.Size))
			}
		}
		tw.Close()
		gz.Close()
		return buf.Bytes()
	}
	for name, data := range map[string][]byte{
		"absolute":  mk(&tar.Header{Name: "/etc/cron.d/x", Size: 1, Typeflag: tar.TypeReg}),
		"traversal": mk(&tar.Header{Name: "../../etc/x", Size: 1, Typeflag: tar.TypeReg}),
		"symlink":   mk(&tar.Header{Name: "etc/x", Linkname: "/etc/shadow", Typeflag: tar.TypeSymlink}),
		"twice":     mk(&tar.Header{Name: "a", Size: 1, Typeflag: tar.TypeReg}, &tar.Header{Name: "a", Size: 1, Typeflag: tar.TypeReg}),
	} {
		if _, _, err := Unpack(data); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, _, err := Unpack([]byte("not gzip")); !errors.Is(err, ErrNotBackup) {
		t.Errorf("not gzip: %v", err)
	}
}

func TestPartsOrder(t *testing.T) {
	// Homestack's own settings go last: restoring them signs everyone out.
	if Parts[len(Parts)-1].ID != "homestack" {
		t.Fatal("homestack must be restored last")
	}
	if p, ok := Info("network"); !ok || p.Default {
		t.Fatal("network must be off by default")
	}
}
