// Package cloudimg downloads official cloud images for VM templates, and
// checks each one against the checksum its publisher lists.
package cloudimg

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

// Image is one entry in the catalog of official cloud images.
type Image struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Family      string `json:"family"` // debian, ubuntu, almalinux, rocky
	Note        string `json:"note"`
	URL         string `json:"url"`
	SumsURL     string `json:"sumsUrl"` // the publisher's checksum list
	ApproxMB    int    `json:"approxMB"`
	Recommended bool   `json:"recommended,omitempty"`
}

// Catalog lists the images offered with one click. All are the publishers'
// "latest" builds, so a new template always starts fully patched.
var Catalog = []Image{
	{ID: "debian-13", Name: "Debian 13 (trixie)", Family: "debian", ApproxMB: 340, Recommended: true,
		Note: "Current stable. Small and minimal, a good default for servers.",
		URL:  "https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2", SumsURL: "https://cloud.debian.org/images/cloud/trixie/latest/SHA512SUMS"},
	{ID: "debian-12", Name: "Debian 12 (bookworm)", Family: "debian", ApproxMB: 350,
		Note: "Previous release, now in long-term support until mid-2028.",
		URL:  "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2", SumsURL: "https://cloud.debian.org/images/cloud/bookworm/latest/SHA512SUMS"},
	{ID: "ubuntu-26.04", Name: "Ubuntu 26.04 LTS", Family: "ubuntu", ApproxMB: 830,
		Note: "Latest long-term support release, supported until 2031.",
		URL:  "https://cloud-images.ubuntu.com/resolute/current/resolute-server-cloudimg-amd64.img", SumsURL: "https://cloud-images.ubuntu.com/resolute/current/SHA256SUMS"},
	{ID: "ubuntu-24.04", Name: "Ubuntu 24.04 LTS", Family: "ubuntu", ApproxMB: 600,
		Note: "Previous LTS, supported until 2029.",
		URL:  "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img", SumsURL: "https://cloud-images.ubuntu.com/noble/current/SHA256SUMS"},
	{ID: "almalinux-10", Name: "AlmaLinux 10", Family: "almalinux", ApproxMB: 480,
		Note: "Red Hat Enterprise Linux compatible.",
		URL:  "https://repo.almalinux.org/almalinux/10/cloud/x86_64/images/AlmaLinux-10-GenericCloud-latest.x86_64.qcow2", SumsURL: "https://repo.almalinux.org/almalinux/10/cloud/x86_64/images/CHECKSUM"},
	{ID: "rocky-10", Name: "Rocky Linux 10", Family: "rocky", ApproxMB: 520,
		Note: "Red Hat Enterprise Linux compatible.",
		URL:  "https://dl.rockylinux.org/pub/rocky/10/images/x86_64/Rocky-10-GenericCloud-Base.latest.x86_64.qcow2", SumsURL: "https://dl.rockylinux.org/pub/rocky/10/images/x86_64/Rocky-10-GenericCloud-Base.latest.x86_64.qcow2.CHECKSUM"},
}

// Find returns a catalog image by id.
func Find(id string) (Image, bool) {
	for _, img := range Catalog {
		if img.ID == id {
			return img, true
		}
	}
	return Image{}, false
}

// MaxSize stops a wrong link from filling the disk.
const MaxSize = 64 << 30

var client = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 20 * time.Second}).DialContext,
		TLSHandshakeTimeout:   20 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ForceAttemptHTTP2:     true,
	},
	// No overall timeout: a big image on a slow line takes a while. The
	// caller's context ends it.
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return errors.New("the download was redirected away from HTTPS; refusing it")
		}
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		return nil
	},
}

// CheckURL accepts only https links to a file.
func CheckURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return nil, errors.New("enter a full link, starting with https://")
	}
	if u.Scheme != "https" {
		return nil, errors.New("use an https:// link, so the download can't be tampered with on the way")
	}
	if u.User != nil {
		return nil, errors.New("links with a username or password in them aren't supported")
	}
	if FileName(u) == "" {
		return nil, errors.New("the link must point to an image file")
	}
	return u, nil
}

// FileName is the last part of a link's path.
func FileName(u *url.URL) string {
	f := path.Base(u.Path)
	if f == "." || f == "/" {
		return ""
	}
	return f
}

func get(ctx context.Context, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Homestack (VM templates)")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("%s answered %s", res.Request.URL.Host, res.Status)
	}
	return res, nil
}

// ExpectedSum fetches a checksum list and returns the entry for file.
func ExpectedSum(ctx context.Context, sumsURL, file string) (string, error) {
	if _, err := CheckURL(sumsURL); err != nil {
		return "", fmt.Errorf("checksum list: %v", err)
	}
	res, err := get(ctx, sumsURL)
	if err != nil {
		return "", fmt.Errorf("couldn't fetch the checksum list: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("couldn't fetch the checksum list: %v", err)
	}
	return ParseSums(string(body), file)
}

var (
	reGNU = regexp.MustCompile(`^([0-9a-fA-F]{64}|[0-9a-fA-F]{128})\s+\*?(.+)$`)
	reBSD = regexp.MustCompile(`^SHA(256|512) \((.+)\) = ([0-9a-fA-F]+)$`)
)

// ParseSums finds file in a checksum list, in either the GNU format
// ("hash  name", Debian and Ubuntu) or the BSD one ("SHA256 (name) = hash").
func ParseSums(text, file string) (string, error) {
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if m := reGNU.FindStringSubmatch(line); m != nil && strings.TrimSpace(m[2]) == file {
			return strings.ToLower(m[1]), nil
		}
		if m := reBSD.FindStringSubmatch(line); m != nil && m[2] == file {
			return strings.ToLower(m[3]), nil
		}
	}
	return "", fmt.Errorf("the checksum list doesn't mention %s", file)
}

// NewHash returns the hash that matches a checksum's length (SHA-256 or SHA-512).
func NewHash(sum string) (hash.Hash, error) {
	sum = strings.ToLower(strings.TrimSpace(sum))
	if _, err := hex.DecodeString(sum); err != nil {
		return nil, errors.New("a checksum is a SHA-256 or SHA-512 value in hex")
	}
	switch len(sum) {
	case 64:
		return sha256.New(), nil
	case 128:
		return sha512.New(), nil
	}
	return nil, errors.New("a checksum is a SHA-256 (64 characters) or SHA-512 (128 characters) value")
}

// Open starts a download and returns its body and size.
func Open(ctx context.Context, u string) (io.ReadCloser, int64, error) {
	res, err := get(ctx, u)
	if err != nil {
		return nil, 0, fmt.Errorf("couldn't download the image: %v", err)
	}
	if res.ContentLength > MaxSize {
		res.Body.Close()
		return nil, 0, fmt.Errorf("that file is %d GB, more than an image should be", res.ContentLength>>30)
	}
	// A wrong link often serves a web page instead of an image.
	if ct := res.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
		res.Body.Close()
		return nil, 0, errors.New("that link serves a web page, not an image file")
	}
	return res.Body, res.ContentLength, nil
}

// Kind tells a disk image's format from its first 512 bytes: qcow2, raw (a
// partitioned disk) or "" for anything else (compressed files, web pages).
func Kind(head []byte) string {
	if len(head) >= 4 && string(head[:4]) == "QFI\xfb" {
		return "qcow2"
	}
	if len(head) >= 512 && head[510] == 0x55 && head[511] == 0xAA {
		return "raw"
	}
	return ""
}
