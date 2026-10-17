// Package backup reads and writes Homestack's backups: a tar.gz of the
// settings files Homestack and its host agent keep, encrypted with a key
// derived from a passphrase, so one file brings a server back after its boot
// drive dies.
//
// A file is "HSBACKUP", a version byte (1), the PBKDF2-SHA256 iteration
// count (uint32, big endian), a 16-byte salt and a 12-byte nonce, then the
// archive sealed with AES-256-GCM, the header as additional data: a changed
// header fails exactly like changed contents or a wrong passphrase.
package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
)

const (
	magic = "HSBACKUP"
	// Iterations is OWASP's 2023 figure for PBKDF2-HMAC-SHA256.
	Iterations = 600_000
	// MaxSize caps a backup (they're a few hundred KB; VMs' UEFI variables
	// are the biggest part).
	MaxSize   = 16 << 20
	headerLen = len(magic) + 1 + 4 + 16 + 12
	version   = 1
)

var (
	// ErrNotBackup is a file that isn't a Homestack backup at all.
	ErrNotBackup = errors.New("this isn't a Homestack backup")
	// ErrPassphrase is a wrong passphrase, or a file that was changed.
	ErrPassphrase = errors.New("the passphrase is wrong, or the file was changed")
)

// Key is what a passphrase turns into. The agent keeps it (not the
// passphrase) to encrypt the nightly backups.
type Key struct {
	Salt       []byte `json:"salt"`
	Iterations int    `json:"iterations"`
	Key        []byte `json:"key"`
}

// NewKey derives a key from a passphrase, with a fresh salt.
func NewKey(passphrase string) (Key, error) {
	if err := CheckPassphrase(passphrase); err != nil {
		return Key{}, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return Key{}, err
	}
	k, err := pbkdf2.Key(sha256.New, passphrase, salt, Iterations, 32)
	if err != nil {
		return Key{}, err
	}
	return Key{Salt: salt, Iterations: Iterations, Key: k}, nil
}

// CheckPassphrase says why a passphrase isn't good enough.
func CheckPassphrase(p string) error {
	if len([]rune(p)) < 12 {
		return errors.New("use at least 12 characters for the backup passphrase (a few words is easiest to remember)")
	}
	return nil
}

// Seal encrypts an archive.
func Seal(k Key, plain []byte) ([]byte, error) {
	if len(k.Key) != 32 || len(k.Salt) != 16 || k.Iterations <= 0 {
		return nil, errors.New("no backup passphrase is set")
	}
	h := make([]byte, 0, headerLen)
	h = append(h, magic...)
	h = append(h, version)
	h = binary.BigEndian.AppendUint32(h, uint32(k.Iterations))
	h = append(h, k.Salt...)
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	h = append(h, nonce...)
	aead, err := gcm(k.Key)
	if err != nil {
		return nil, err
	}
	return aead.Seal(h, nonce, plain, h), nil
}

// Open decrypts a backup with its passphrase.
func Open(data []byte, passphrase string) ([]byte, error) {
	if len(data) < headerLen || string(data[:len(magic)]) != magic {
		return nil, ErrNotBackup
	}
	if len(data) > MaxSize {
		return nil, fmt.Errorf("the file is bigger than a backup can be (%d MB)", MaxSize>>20)
	}
	h := data[:headerLen]
	if h[len(magic)] != version {
		return nil, fmt.Errorf("this backup is from a newer Homestack (format %d); update Homestack first", h[len(magic)])
	}
	iter := int(binary.BigEndian.Uint32(h[len(magic)+1:]))
	if iter < 100_000 || iter > 10_000_000 { // made by Homestack, so neither
		return nil, ErrNotBackup
	}
	salt := h[len(magic)+5 : len(magic)+21]
	nonce := h[len(magic)+21:]
	k, err := pbkdf2.Key(sha256.New, passphrase, salt, iter, 32)
	if err != nil {
		return nil, err
	}
	aead, err := gcm(k)
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, nonce, data[headerLen:], h)
	if err != nil {
		return nil, ErrPassphrase
	}
	return plain, nil
}

func gcm(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// ---------- The archive ----------

// Entry is one file in a backup.
type Entry struct {
	Name string // relative, slash-separated: etc/snapraid.conf
	Mode int64
	Data []byte
}

// Pack makes the tar.gz of entries, the manifest first.
func Pack(m Manifest, entries []Entry) ([]byte, error) {
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range append([]Entry{{Name: ManifestName, Mode: 0o600, Data: mb}}, entries...) {
		if !validName(e.Name) {
			return nil, fmt.Errorf("bad name in backup: %q", e.Name)
		}
		hdr := &tar.Header{Name: e.Name, Mode: e.Mode, Size: int64(len(e.Data)), ModTime: m.Created, Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(e.Data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Unpack reads an archive: its manifest and files. Only plain files with
// plain relative names are accepted, so nothing in an archive can name a
// place to be written; where files go is decided by whoever restores them.
func Unpack(data []byte) (Manifest, map[string]Entry, error) {
	var m Manifest
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return m, nil, ErrNotBackup
	}
	tr := tar.NewReader(io.LimitReader(gz, 4*MaxSize))
	files := map[string]Entry{}
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return m, nil, fmt.Errorf("the backup is damaged: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg || !validName(hdr.Name) {
			return m, nil, fmt.Errorf("the backup has an unexpected entry %q", hdr.Name)
		}
		if total += hdr.Size; total > 4*MaxSize {
			return m, nil, errors.New("the backup unpacks to more than a backup can hold")
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return m, nil, fmt.Errorf("the backup is damaged: %v", err)
		}
		if _, dup := files[hdr.Name]; dup {
			return m, nil, fmt.Errorf("the backup has %q twice", hdr.Name)
		}
		files[hdr.Name] = Entry{Name: hdr.Name, Mode: hdr.Mode, Data: b}
	}
	mf, ok := files[ManifestName]
	if !ok {
		return m, nil, errors.New("the backup has no manifest")
	}
	if err := json.Unmarshal(mf.Data, &m); err != nil || m.Format != 1 {
		return m, nil, errors.New("the backup's manifest isn't readable")
	}
	delete(files, ManifestName)
	return m, files, nil
}

func validName(n string) bool {
	return n != "" && !strings.HasPrefix(n, "/") && path.Clean(n) == n && n != ".." && !strings.HasPrefix(n, "../") && !strings.Contains(n, "\\")
}

// ---------- What's in it ----------

// ManifestName is the archive's first entry.
const ManifestName = "manifest.json"

// Manifest describes a backup.
type Manifest struct {
	Format  int       `json:"format"`
	Created time.Time `json:"created"`
	Host    string    `json:"host"`
	Version string    `json:"version"` // Homestack's
	Parts   []Part    `json:"parts"`
	Missing []string  `json:"missing,omitempty"` // what couldn't be read
}

// Part is one thing a backup can restore, and its files in the archive.
type Part struct {
	ID    string   `json:"id"`
	Files []string `json:"files"`
	Note  string   `json:"note,omitempty"` // 3 VMs, 2 share users…
}

// PartInfo describes a part for the restore checklist, in restore order.
type PartInfo struct {
	ID, Label, Desc string
	Default         bool // ticked to begin with
	Host            bool // restored by the host agent
}

// Parts are the parts, in the order they're restored.
var Parts = []PartInfo{
	{"storage", "The array", "Where the storage is mounted (Settings → Storage), snapraid.conf, the storage's mounts in /etc/fstab (only those), the ZFS pools (by their IDs) and the ZFS cache limit.", true, true},
	{"system", "System settings", "Docker's settings (/etc/docker/daemon.json) and the drop-ins that make Docker and the VMs wait for the array at boot. Docker uses them from its next restart.", true, true},
	{"shares", "Shares and their users", "Samba's settings, the shares, and the share users with their passwords and user IDs, so file ownership on the array still matches.", true, true},
	{"schedules", "Schedules and alerts", "When the checks, the mover, parity and backups run; the alert settings and the mover's.", true, true},
	{"fans", "Fan curves", "The fan curves and the fan chip's driver.", true, true},
	{"ups", "UPS", "The UPS settings (NUT).", true, true},
	{"vms", "Virtual machines", "Their definitions and UEFI variables. Their disks are on the array, so they're not in the backup.", true, true},
	{"network", "Network", "Interfaces, bond, bridge and VLANs (systemd-networkd). Only on the same hardware: with other network cards it can cut the server off. Takes effect at the next restart.", false, true},
	{"homestack", "Homestack's account and settings", "Your login, settings, notification channels, app links and VM templates. Signs everyone out.", true, false},
}

// Info finds a part's description.
func Info(id string) (PartInfo, bool) {
	for _, p := range Parts {
		if p.ID == id {
			return p, true
		}
	}
	return PartInfo{}, false
}
