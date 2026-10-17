// Package cloudinit builds the first-boot settings for VMs made from cloud
// images: a NoCloud seed disk that sets the hostname, the login, SSH keys
// and the network.
//
// The files are written as JSON, which YAML parsers (cloud-init's included)
// read as YAML, so no value can break out of its field.
package cloudinit

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"time"
)

// Config is what a new VM is set up with on its first boot.
type Config struct {
	Hostname    string   `json:"hostname"`
	User        string   `json:"user"`
	Password    string   `json:"password,omitempty"` // hashed before it goes anywhere, never stored
	SSHKeys     []string `json:"sshKeys"`
	SSHPassword bool     `json:"sshPassword"` // allow password logins over SSH
	Network     Network  `json:"network"`
	Upgrade     bool     `json:"upgrade"`    // install updates on first boot
	GuestAgent  bool     `json:"guestAgent"` // install the QEMU guest agent
	Timezone    string   `json:"timezone,omitempty"`
}

// Network is the VM's first network card.
type Network struct {
	Mode    string   `json:"mode"`    // dhcp | static
	Address string   `json:"address"` // 192.168.1.50/24
	Gateway string   `json:"gateway"`
	DNS     []string `json:"dns"`
}

var (
	reLabel   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	reUser    = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	reTZ      = regexp.MustCompile(`^[A-Za-z]+(/[A-Za-z0-9_+-]+){0,2}$|^UTC$`)
	reKeyLine = regexp.MustCompile(`^((?:sk-)?(?:ssh|ecdsa)-[a-z0-9-]+(?:@openssh\.com)?) ([A-Za-z0-9+/]+={0,2})(?: (.*))?$`)
)

// Accounts that already exist on Linux systems, or would be dangerous to take over.
var reservedUsers = map[string]bool{
	"root": true, "daemon": true, "bin": true, "sys": true, "sync": true, "games": true, "man": true,
	"lp": true, "mail": true, "news": true, "uucp": true, "proxy": true, "www-data": true, "backup": true,
	"list": true, "irc": true, "nobody": true, "messagebus": true, "sshd": true, "polkitd": true,
	"systemd-network": true, "systemd-resolve": true, "systemd-timesync": true, "_apt": true,
	"adm": true, "operator": true, "ftp": true, "halt": true, "shutdown": true,
}

// HostnameFrom turns a VM name into a valid hostname, as a default.
func HostnameFrom(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0:
			b.WriteByte('-')
		}
	}
	h := strings.Trim(b.String(), "-")
	if len(h) > 63 {
		h = strings.Trim(h[:63], "-")
	}
	return h
}

// Normalize trims the input and drops blank SSH key lines.
func (c *Config) Normalize() {
	c.Hostname = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(c.Hostname), "."))
	c.User = strings.TrimSpace(c.User)
	var keys []string
	for _, k := range c.SSHKeys {
		for line := range strings.SplitSeq(k, "\n") {
			if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
				keys = append(keys, line)
			}
		}
	}
	c.SSHKeys = keys
	c.Network.Mode = strings.TrimSpace(c.Network.Mode)
	c.Network.Address = strings.TrimSpace(c.Network.Address)
	c.Network.Gateway = strings.TrimSpace(c.Network.Gateway)
	var dns []string
	for _, d := range c.Network.DNS {
		for f := range strings.FieldsFuncSeq(d, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
			dns = append(dns, f)
		}
	}
	c.Network.DNS = dns
}

// Validate checks everything, so a VM is never created that nobody can log in to.
func (c *Config) Validate() error {
	labels := strings.Split(c.Hostname, ".")
	if c.Hostname == "" || len(c.Hostname) > 253 {
		return errors.New("enter a hostname")
	}
	for _, l := range labels {
		if !reLabel.MatchString(l) {
			return fmt.Errorf("%q isn't a valid hostname: use lower-case letters, numbers and dashes", c.Hostname)
		}
	}
	if !reUser.MatchString(c.User) {
		return errors.New("the username must start with a letter and use lower-case letters, numbers, dashes and underscores (32 at most)")
	}
	if reservedUsers[c.User] || strings.HasPrefix(c.User, "systemd-") {
		return fmt.Errorf("%s is a system account; pick another username", c.User)
	}
	if c.Password != "" {
		if len(c.Password) < 8 || len(c.Password) > 256 {
			return errors.New("use a password of at least 8 characters")
		}
		if strings.ContainsAny(c.Password, "\r\n\x00") {
			return errors.New("the password can't contain line breaks")
		}
	}
	for _, k := range c.SSHKeys {
		if err := checkKey(k); err != nil {
			return err
		}
	}
	if c.Password == "" && len(c.SSHKeys) == 0 {
		return errors.New("add an SSH key or a password, or you won't be able to log in")
	}
	if c.SSHPassword && c.Password == "" {
		return errors.New("SSH password logins need a password")
	}
	if c.Timezone != "" && !reTZ.MatchString(c.Timezone) {
		c.Timezone = ""
	}
	switch c.Network.Mode {
	case "", "dhcp":
	case "static":
		p, err := netip.ParsePrefix(c.Network.Address)
		if err != nil || p.Bits() == p.Addr().BitLen() || p.Bits() < 8 {
			return errors.New("enter the static address with its prefix length, like 192.168.1.50/24")
		}
		if p.Addr() == p.Masked().Addr() {
			return fmt.Errorf("%s is the network's own address; pick an address inside it", c.Network.Address)
		}
		if a := p.Addr(); a.Is4() && p.Bits() <= 30 {
			v := binary.BigEndian.Uint32(a.AsSlice())
			if v|(1<<(32-p.Bits())-1) == v {
				return fmt.Errorf("%s is the network's broadcast address; pick an address inside it", c.Network.Address)
			}
		}
		if c.Network.Gateway != "" {
			gw, err := netip.ParseAddr(c.Network.Gateway)
			if err != nil {
				return fmt.Errorf("%q isn't an IP address", c.Network.Gateway)
			}
			if !p.Contains(gw) {
				return fmt.Errorf("the gateway %s isn't on %s, so the VM couldn't reach it", gw, p.Masked())
			}
			if gw == p.Addr() {
				return errors.New("the gateway can't be the VM's own address")
			}
		}
		for _, d := range c.Network.DNS {
			if _, err := netip.ParseAddr(d); err != nil {
				return fmt.Errorf("%q isn't a DNS server address", d)
			}
		}
	default:
		return errors.New("unknown network mode")
	}
	return nil
}

func checkKey(k string) error {
	if strings.Contains(k, "PRIVATE KEY") {
		return errors.New("that's a private key. Paste the public key instead (the .pub file), and keep the private key secret")
	}
	m := reKeyLine.FindStringSubmatch(k)
	short := k
	if len(short) > 40 {
		short = short[:40] + "…"
	}
	if m == nil {
		return fmt.Errorf("%q isn't an SSH public key; they look like: ssh-ed25519 AAAA… you@pc", short)
	}
	blob, err := base64.StdEncoding.DecodeString(m[2])
	if err != nil || len(blob) < 4 {
		return fmt.Errorf("the SSH key %q is damaged (incomplete copy?)", short)
	}
	n := binary.BigEndian.Uint32(blob)
	if int(n) > len(blob)-4 || string(blob[4:4+n]) != m[1] {
		return fmt.Errorf("the SSH key %q is damaged (incomplete copy?)", short)
	}
	return nil
}

// Seed builds the seed disk for a VM whose network card has the given MAC
// (empty when it has none). Validate first.
func Seed(c Config, mac string) ([]byte, error) {
	files, err := c.files(mac)
	if err != nil {
		return nil, err
	}
	return seedISO("cidata", files, time.Now())
}

func (c Config) files(mac string) ([]isoFile, error) {
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	host, _, _ := strings.Cut(c.Hostname, ".")
	meta := map[string]any{"instance-id": "homestack-" + hex.EncodeToString(id), "local-hostname": host}

	user := map[string]any{
		"name": c.User,
		//		"shell": "/bin/bash", !!!Dont hardcode the shell!!!!!
		// A sudoers rule works on every distro, unlike the group (sudo or wheel).
		"sudo":        "ALL=(ALL) NOPASSWD:ALL",
		"lock_passwd": c.Password == "",
	}
	if c.Password != "" {
		hash, err := HashPassword(c.Password)
		if err != nil {
			return nil, err
		}
		user["passwd"] = hash
		user["sudo"] = "ALL=(ALL) ALL"
	}
	if len(c.SSHKeys) > 0 {
		user["ssh_authorized_keys"] = c.SSHKeys
	}
	ud := map[string]any{
		"hostname":         host,
		"manage_etc_hosts": true,
		"users":            []any{user},
		"disable_root":     true,
		"ssh_pwauth":       c.SSHPassword,
		"package_update":   c.Upgrade || c.GuestAgent,
		"package_upgrade":  c.Upgrade,
	}
	if host != c.Hostname {
		ud["fqdn"] = c.Hostname
	}
	if c.Timezone != "" {
		ud["timezone"] = c.Timezone
	}
	pkgs := []string{"sudo"} // Alpine images may ship doas instead
	if c.GuestAgent {
		pkgs = append(pkgs, "qemu-guest-agent")
		ud["runcmd"] = [][]string{{"sh", "-c",
			"if command -v systemctl >/dev/null 2>&1; then systemctl enable --now qemu-guest-agent; " +
				"else rc-update add qemu-guest-agent default && rc-service qemu-guest-agent start; fi"}}
	}
	ud["packages"] = pkgs
	files := []isoFile{}
	add := func(name, prefix string, v any) error {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		files = append(files, isoFile{name, append([]byte(prefix), append(b, '\n')...)})
		return nil
	}
	if err := add("meta-data", "", meta); err != nil {
		return nil, err
	}
	if err := add("user-data", "#cloud-config\n", ud); err != nil {
		return nil, err
	}
	if mac != "" {
		nic := map[string]any{"match": map[string]string{"macaddress": strings.ToLower(mac)}}
		if c.Network.Mode == "static" {
			p := netip.MustParsePrefix(c.Network.Address)
			nic["addresses"] = []string{p.String()}
			if c.Network.Gateway != "" {
				to := "0.0.0.0/0"
				if p.Addr().Is6() {
					to = "::/0"
				}
				nic["routes"] = []map[string]string{{"to": to, "via": c.Network.Gateway}}
			}
			dns := c.Network.DNS
			if len(dns) == 0 && c.Network.Gateway != "" {
				dns = []string{c.Network.Gateway} // most home routers answer DNS
			}
			if len(dns) > 0 {
				nic["nameservers"] = map[string]any{"addresses": dns}
			}
		} else {
			nic["dhcp4"] = true
		}
		if err := add("network-config", "", map[string]any{"version": 2, "ethernets": map[string]any{"nic0": nic}}); err != nil {
			return nil, err
		}
	}
	return files, nil
}
