package cloudimg

import (
	"net/url"
	"strings"
	"testing"
)

func TestParseSums(t *testing.T) {
	sha512 := strings.Repeat("ab", 64)
	sha256 := strings.Repeat("cd", 32)
	cases := []struct{ name, text, file, want string }{
		{"debian", sha512 + "  debian-13-genericcloud-amd64.json\n" + strings.Repeat("ef", 64) + "  debian-13-genericcloud-amd64.qcow2\n",
			"debian-13-genericcloud-amd64.qcow2", strings.Repeat("ef", 64)},
		{"ubuntu binary marker", sha256 + " *resolute-server-cloudimg-amd64.img\n", "resolute-server-cloudimg-amd64.img", sha256},
		{"rocky bsd", "# Rocky-10-GenericCloud-Base.latest.x86_64.qcow2: 544997376 bytes\nSHA256 (Rocky-10-GenericCloud-Base.latest.x86_64.qcow2) = " + strings.ToUpper(sha256) + "\n",
			"Rocky-10-GenericCloud-Base.latest.x86_64.qcow2", sha256},
		{"exact name only", sha256 + "  AlmaLinux-10-GenericCloud-ext4-latest.x86_64.qcow2\n" + sha512[:64] + "  AlmaLinux-10-GenericCloud-latest.x86_64.qcow2\n",
			"AlmaLinux-10-GenericCloud-latest.x86_64.qcow2", sha512[:64]},
	}
	for _, c := range cases {
		got, err := ParseSums(c.text, c.file)
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v", c.name, got, err)
		}
	}
	if _, err := ParseSums(sha256+"  other.img\n", "debian.qcow2"); err == nil {
		t.Error("a missing entry must be an error")
	}
}

func TestNewHash(t *testing.T) {
	if h, err := NewHash(strings.Repeat("a", 64)); err != nil || h.Size() != 32 {
		t.Fatal("sha256")
	}
	if h, err := NewHash(strings.Repeat("A", 128)); err != nil || h.Size() != 64 {
		t.Fatal("sha512")
	}
	for _, bad := range []string{"", "abc", strings.Repeat("g", 64), strings.Repeat("a", 40)} {
		if _, err := NewHash(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestCheckURL(t *testing.T) {
	for _, bad := range []string{"http://example.com/a.qcow2", "ftp://x/a", "https://", "example.com/a.qcow2", "https://u:p@example.com/a.qcow2", "https://example.com/"} {
		if _, err := CheckURL(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	u, err := CheckURL(" https://example.com/images/a.qcow2?x=1 ")
	if err != nil || FileName(u) != "a.qcow2" {
		t.Fatalf("%v %v", u, err)
	}
	for _, img := range Catalog {
		u, err := url.Parse(img.URL)
		if err != nil || u.Scheme != "https" || !strings.HasPrefix(img.SumsURL, "https://") {
			t.Errorf("%s: catalog links must be https", img.ID)
		}
	}
}

func TestKind(t *testing.T) {
	mbr := make([]byte, 512)
	mbr[510], mbr[511] = 0x55, 0xAA
	if Kind([]byte("QFI\xfb\x00\x00\x00\x03")) != "qcow2" || Kind(mbr) != "raw" || Kind([]byte("<!doctype html>")) != "" || Kind([]byte{0xfd, '7', 'z', 'X', 'Z', 0}) != "" {
		t.Fatal("Kind")
	}
}
