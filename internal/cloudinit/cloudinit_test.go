package cloudinit

import (
	"encoding/binary"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestSHA512Crypt(t *testing.T) {
	// Test vectors from the specification.
	cases := []struct {
		key, salt string
		rounds    int
		explicit  bool
		want      string
	}{
		{"Hello world!", "saltstring", 5000, false,
			"$6$saltstring$svn8UoSVapNtMuq1ukKS4tPQd8iKwSMHWjl/O817G3uBnIFNjnQJuesI68u4OTLiBFdcbYEdFCoEOfaS35inz1"},
		{"Hello world!", "saltstringsaltstring", 10000, true,
			"$6$rounds=10000$saltstringsaltst$OW1/O6BYHV6BcXZu8QVeXbDWra3Oeqh0sbHbbMCVNSnCM/UrjmM0Dp8vOuZeHBy/YTBmSK6H9qs/y3RnOaw5v."},
		{"This is just a test", "toolongsaltstring", 5000, true,
			"$6$rounds=5000$toolongsaltstrin$lQ8jolhgVRVhY4b5pZKaysCLi0QBxGoNeKQzQ3glMhwllF7oGDZxUhx1yxdYcz/e1JSbq3y6JMxxl8audkUEm0"},
		{"a very much longer text to encrypt.  This one even stretches over morethan one line.", "anotherlongsaltstring", 1400, true,
			"$6$rounds=1400$anotherlongsalts$POfYwTEok97VWcjxIiSOjiykti.o/pQs.wPvMxQ6Fm7I6IoYN3CmLs66x9t0oSwbtEW7o7UmJEiDwGqd8p4ur1"},
		{"we have a short salt string but not a short password", "short", 77777, true,
			"$6$rounds=77777$short$WuQyW2YR.hBNpjjRhpYD/ifIw05xdfeEyQoMxIXbkvr0gge1a1x3yRULJ5CCaUeOxFmtlcGZelFl5CxtgfiAc0"},
		{"the minimum number is still observed", "roundstoolow", 10, true,
			"$6$rounds=1000$roundstoolow$kUMsbe306n21p9R.FRkW3IGn.S9NPN0x50YhH1xhLsPuWGsUSklZt58jaTfF4ZEQpyUNGc0dqbpBYYBaHHrsX."},
	}
	for _, c := range cases {
		if got := sha512Crypt([]byte(c.key), []byte(c.salt), c.rounds, c.explicit); got != c.want {
			t.Errorf("sha512Crypt(%q, %q)\n got %s\nwant %s", c.key, c.salt, got, c.want)
		}
	}
	h, err := HashPassword("correct horse")
	if err != nil || !strings.HasPrefix(h, "$6$rounds=100000$") || len(strings.Split(h, "$")) != 5 {
		t.Fatalf("HashPassword = %q, %v", h, err)
	}
	parts := strings.Split(h, "$")
	if again := sha512Crypt([]byte("correct horse"), []byte(parts[3]), 100000, true); again != h {
		t.Fatalf("hash doesn't verify: %s vs %s", again, h)
	}
}

// readISO lists the files in the Joliet tree of an image built by seedISO,
// the way Linux sees them.
func readISO(t *testing.T, img []byte) (label string, files map[string]string) {
	t.Helper()
	if len(img)%sector != 0 {
		t.Fatalf("image size %d isn't whole sectors", len(img))
	}
	pvd := img[16*sector:]
	if pvd[0] != 1 || string(pvd[1:6]) != "CD001" {
		t.Fatal("no primary volume descriptor")
	}
	if got := binary.LittleEndian.Uint32(pvd[80:]); int(got)*sector != len(img) {
		t.Fatalf("volume size says %d sectors, image has %d", got, len(img)/sector)
	}
	svd := img[17*sector:]
	if svd[0] != 2 || string(svd[88:91]) != "%/E" {
		t.Fatal("no Joliet descriptor")
	}
	if img[18*sector] != 255 {
		t.Fatal("no terminator")
	}
	decode := func(b []byte) string {
		u := make([]uint16, len(b)/2)
		for i := range u {
			u[i] = binary.BigEndian.Uint16(b[2*i:])
		}
		return strings.TrimRight(string(utf16.Decode(u)), " ")
	}
	label = decode(svd[40:72])
	if p := strings.TrimRight(string(pvd[40:72]), " "); p != label {
		t.Fatalf("labels differ: %q and %q", p, label)
	}
	root := svd[156:]
	ext := binary.LittleEndian.Uint32(root[2:])
	dir := img[int(ext)*sector : int(ext+1)*sector]
	files = map[string]string{}
	var last string
	for off := 0; off < len(dir) && dir[off] != 0; off += int(dir[off]) {
		r := dir[off:]
		if r[0]%2 != 0 {
			t.Fatalf("odd record length %d", r[0])
		}
		idLen := int(r[32])
		id := r[33 : 33+idLen]
		if idLen == 1 && id[0] <= 1 {
			continue
		}
		name := strings.TrimSuffix(decode(id), ";1")
		if name < last {
			t.Fatalf("directory isn't sorted: %s after %s", name, last)
		}
		last = name
		at := binary.LittleEndian.Uint32(r[2:])
		size := binary.LittleEndian.Uint32(r[10:])
		if binary.BigEndian.Uint32(r[6:]) != at || binary.BigEndian.Uint32(r[14:]) != size {
			t.Fatalf("%s: both-endian fields disagree", name)
		}
		files[name] = string(img[int(at)*sector : int(at)*sector+int(size)])
	}
	return label, files
}

func TestSeedISO(t *testing.T) {
	big := strings.Repeat("x", 5000) // spans three sectors
	img, err := seedISO("cidata", []isoFile{{"user-data", []byte(big)}, {"meta-data", []byte("m")}, {"network-config", nil}}, time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	label, files := readISO(t, img)
	if label != "cidata" {
		t.Fatalf("label %q", label)
	}
	if files["user-data"] != big || files["meta-data"] != "m" || len(files) != 3 {
		t.Fatalf("files: %v", len(files))
	}
	if got := shortName("network-config"); got != "NETWORK_.;1" {
		t.Fatalf("shortName = %s", got)
	}
}

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl you@pc"

func validConfig() Config {
	return Config{Hostname: "web01", User: "andrew", SSHKeys: []string{testKey}, Network: Network{Mode: "dhcp"}}
}

func TestValidate(t *testing.T) {
	bad := map[string]func(c *Config){
		"no login":           func(c *Config) { c.SSHKeys = nil },
		"root":               func(c *Config) { c.User = "root" },
		"upper user":         func(c *Config) { c.User = "Andrew" },
		"bad hostname":       func(c *Config) { c.Hostname = "web_01" },
		"private key":        func(c *Config) { c.SSHKeys = []string{"-----BEGIN OPENSSH PRIVATE KEY-----"} },
		"truncated key":      func(c *Config) { c.SSHKeys = []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqq"} },
		"wrong type in blob": func(c *Config) { c.SSHKeys = []string{strings.Replace(testKey, "ssh-ed25519", "ssh-rsa", 1)} },
		"short password":     func(c *Config) { c.Password = "short" },
		"ssh pw no pw":       func(c *Config) { c.SSHPassword = true },
		"static no prefix":   func(c *Config) { c.Network = Network{Mode: "static", Address: "192.168.1.50"} },
		"static net addr":    func(c *Config) { c.Network = Network{Mode: "static", Address: "192.168.1.0/24"} },
		"static broadcast":   func(c *Config) { c.Network = Network{Mode: "static", Address: "192.168.1.255/24"} },
		"gateway off net":    func(c *Config) { c.Network = Network{Mode: "static", Address: "192.168.1.50/24", Gateway: "10.0.0.1"} },
		"gateway is self": func(c *Config) {
			c.Network = Network{Mode: "static", Address: "192.168.1.50/24", Gateway: "192.168.1.50"}
		},
		"bad dns": func(c *Config) {
			c.Network = Network{Mode: "static", Address: "192.168.1.50/24", DNS: []string{"dns.google"}}
		},
	}
	for name, mutate := range bad {
		c := validConfig()
		mutate(&c)
		c.Normalize()
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c := validConfig()
	c.SSHKeys = []string{testKey + "\n\n# comment\n" + testKey}
	c.Network = Network{Mode: "static", Address: "192.168.1.50/24", Gateway: "192.168.1.1", DNS: []string{"1.1.1.1, 9.9.9.9"}}
	c.Normalize()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(c.SSHKeys) != 2 || len(c.Network.DNS) != 2 {
		t.Fatalf("normalize: %d keys, %v", len(c.SSHKeys), c.Network.DNS)
	}
	if got := HostnameFrom("My_Web.Server-01"); got != "my-web-server-01" {
		t.Fatalf("HostnameFrom = %q", got)
	}
}

func TestSeedFiles(t *testing.T) {
	c := validConfig()
	c.Password = "a long password"
	c.GuestAgent = true
	c.Network = Network{Mode: "static", Address: "192.168.1.50/24", Gateway: "192.168.1.1"}
	c.Normalize()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	img, err := Seed(c, "52:54:00:AB:CD:EF")
	if err != nil {
		t.Fatal(err)
	}
	_, files := readISO(t, img)
	ud := files["user-data"]
	body, ok := strings.CutPrefix(ud, "#cloud-config\n")
	if !ok {
		t.Fatalf("user-data must start with #cloud-config: %q", ud[:20])
	}
	var u struct {
		Hostname string `json:"hostname"`
		Users    []struct {
			Name   string   `json:"name"`
			Passwd string   `json:"passwd"`
			Keys   []string `json:"ssh_authorized_keys"`
			Sudo   string   `json:"sudo"`
		} `json:"users"`
		Packages []string `json:"packages"`
		PwAuth   bool     `json:"ssh_pwauth"`
	}
	if err := json.Unmarshal([]byte(body), &u); err != nil {
		t.Fatal(err)
	}
	if u.Hostname != "web01" || len(u.Users) != 1 || u.Users[0].Name != "andrew" || u.Users[0].Keys[0] != testKey || u.PwAuth {
		t.Fatalf("user-data: %+v", u)
	}
	if !strings.HasPrefix(u.Users[0].Passwd, "$6$") || strings.Contains(ud, "a long password") {
		t.Fatal("the password must only be stored hashed")
	}
	if u.Users[0].Sudo != "ALL=(ALL) ALL" || !slices.Equal(u.Packages, []string{"sudo", "qemu-guest-agent"}) {
		t.Fatalf("sudo %q packages %v", u.Users[0].Sudo, u.Packages)
	}
	var n struct {
		Ethernets map[string]struct {
			Match       map[string]string   `json:"match"`
			Addresses   []string            `json:"addresses"`
			Routes      []map[string]string `json:"routes"`
			Nameservers struct {
				Addresses []string `json:"addresses"`
			} `json:"nameservers"`
		} `json:"ethernets"`
	}
	if err := json.Unmarshal([]byte(files["network-config"]), &n); err != nil {
		t.Fatal(err)
	}
	nic := n.Ethernets["nic0"]
	if nic.Match["macaddress"] != "52:54:00:ab:cd:ef" || nic.Addresses[0] != "192.168.1.50/24" || nic.Routes[0]["via"] != "192.168.1.1" || nic.Nameservers.Addresses[0] != "192.168.1.1" {
		t.Fatalf("network-config: %+v", nic)
	}
	if !strings.Contains(files["meta-data"], `"instance-id": "homestack-`) {
		t.Fatalf("meta-data: %s", files["meta-data"])
	}
	// Keys only: no password, passwordless sudo, no network file without a NIC.
	c = validConfig()
	img, _ = Seed(c, "")
	_, files = readISO(t, img)
	if _, ok := files["network-config"]; ok || !strings.Contains(files["user-data"], `"lock_passwd": true`) || !strings.Contains(files["user-data"], "NOPASSWD") {
		t.Fatalf("keys-only seed: %v", files)
	}
}
