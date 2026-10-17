package libvirt

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// VM templates: disk images that new VMs are cloned from, like Proxmox
// templates. A linked clone is a thin qcow2 file that reads anything it
// hasn't changed from the template, so it's made instantly and starts small;
// a full clone is an independent copy. Everything goes through libvirt's
// storage pools, so Homestack never needs the folders mounted.

// VolInfo describes a disk image.
type VolInfo struct {
	Path       string `json:"path"`
	Format     string `json:"format"`
	Capacity   uint64 `json:"capacity"`   // virtual size, bytes
	Allocation uint64 `json:"allocation"` // space used on disk, bytes
	Backing    string `json:"backing,omitempty"`
}

type volXML struct {
	Capacity struct {
		Value uint64 `xml:",chardata"`
		Unit  string `xml:"unit,attr"`
	} `xml:"capacity"`
	Allocation struct {
		Value uint64 `xml:",chardata"`
		Unit  string `xml:"unit,attr"`
	} `xml:"allocation"`
	Target struct {
		Path   string `xml:"path"`
		Format struct {
			Type string `xml:"type,attr"`
		} `xml:"format"`
	} `xml:"target"`
	Backing struct {
		Path string `xml:"path"`
	} `xml:"backingStore"`
}

func (c *Client) volInfo(ctx context.Context, pool, file string) (VolInfo, error) {
	out, err := c.run(ctx, "vol-dumpxml", "--pool", pool, file)
	if err != nil {
		return VolInfo{}, err
	}
	var v volXML
	if err := xml.Unmarshal([]byte(out), &v); err != nil {
		return VolInfo{}, err
	}
	unit := func(u string) string { return firstNonEmpty(u, "bytes") }
	return VolInfo{
		Path: v.Target.Path, Format: v.Target.Format.Type, Backing: v.Backing.Path,
		Capacity: toBytes(v.Capacity.Value, unit(v.Capacity.Unit)), Allocation: toBytes(v.Allocation.Value, unit(v.Allocation.Unit)),
	}, nil
}

// VolumesIn reads several images in one folder (one pool lookup for all).
// Files that are missing are left out.
func (c *Client) VolumesIn(ctx context.Context, dir string, files []string) (map[string]VolInfo, error) {
	res := map[string]VolInfo{}
	err := c.withDirPool(ctx, dir, func(pool string) error {
		for _, f := range files {
			if v, err := c.volInfo(ctx, pool, f); err == nil {
				res[f] = v
			}
		}
		return nil
	})
	return res, err
}

// VolumeInfo reads one image's format, sizes and backing file.
func (c *Client) VolumeInfo(ctx context.Context, path string) (VolInfo, error) {
	dir, file := splitPath(path)
	var info VolInfo
	err := c.withDirPool(ctx, dir, func(pool string) error {
		var err error
		info, err = c.volInfo(ctx, pool, file)
		return err
	})
	return info, err
}

// Upload streams r into a new file at path, and returns what libvirt makes
// of it. The file is removed again if anything goes wrong.
func (c *Client) Upload(ctx context.Context, path string, r io.Reader) (VolInfo, error) {
	dir, file := splitPath(path)
	var info VolInfo
	err := c.withDirPool(ctx, dir, func(pool string) error {
		if _, err := c.run(ctx, "vol-info", "--pool", pool, file); err == nil {
			return fmt.Errorf("%s already exists", path)
		}
		// An empty file that the upload fills; libvirt works out its format afterwards.
		if _, err := c.run(ctx, "vol-create-as", pool, file, "0", "--format", "raw", "--allocation", "0"); err != nil {
			return err
		}
		_, err := c.runInput(ctx, r, "vol-upload", "--pool", pool, file, "/dev/stdin")
		if err == nil {
			_, _ = c.run(ctx, "pool-refresh", "--", pool)
			info, err = c.volInfo(ctx, pool, file)
		}
		if err != nil {
			_, _ = c.run(context.Background(), "vol-delete", "--pool", pool, file)
		}
		return err
	})
	return info, err
}

// CloneDisk makes dst from the template image src: a linked clone that
// reads unchanged data from src, or a full copy. size (bytes) can grow it.
func (c *Client) CloneDisk(ctx context.Context, src VolInfo, dst string, size uint64, linked bool) error {
	if size < src.Capacity {
		size = src.Capacity
	}
	dstDir, dstFile := splitPath(dst)
	srcDir, srcFile := splitPath(src.Path)
	vol := fmt.Sprintf("<volume><name>%s</name><capacity unit='bytes'>%d</capacity><allocation unit='bytes'>0</allocation>"+
		"<target><format type='qcow2'/></target>", esc(dstFile), size)
	if linked {
		vol += fmt.Sprintf("<backingStore><path>%s</path><format type='%s'/></backingStore>", esc(src.Path), esc(src.Format))
	}
	vol += "</volume>"
	f, err := writeTemp(vol)
	if err != nil {
		return err
	}
	defer os.Remove(f)
	return c.withDirPool(ctx, dstDir, func(pool string) error {
		if _, err := c.run(ctx, "vol-info", "--pool", pool, dstFile); err == nil {
			return fmt.Errorf("%s already exists", dst)
		}
		if linked {
			_, err := c.run(ctx, "vol-create", pool, f)
			return err
		}
		return c.withDirPool(ctx, srcDir, func(srcPool string) error {
			if _, err := c.run(ctx, "vol-create-from", pool, f, "--inputpool", srcPool, srcFile); err != nil {
				return err
			}
			// vol-create-from keeps the source's size; grow it to what was asked.
			if info, err := c.volInfo(ctx, pool, dstFile); err == nil && info.Capacity < size {
				if _, err := c.run(ctx, "vol-resize", "--pool", pool, dstFile, fmt.Sprint(size)); err != nil {
					_, _ = c.run(context.Background(), "vol-delete", "--pool", pool, dstFile)
					return fmt.Errorf("couldn't grow the copy: %v", err)
				}
			}
			return nil
		})
	})
}

// CopyToTemplate writes a full, independent qcow2 copy of a VM's disk to
// dst. A linked clone is flattened, so the copy doesn't depend on anything.
func (c *Client) CopyToTemplate(ctx context.Context, src VolInfo, dst string) (VolInfo, error) {
	dstDir, dstFile := splitPath(dst)
	srcDir, srcFile := splitPath(src.Path)
	f, err := writeTemp(fmt.Sprintf("<volume><name>%s</name><capacity unit='bytes'>%d</capacity>"+
		"<target><format type='qcow2'/></target></volume>", esc(dstFile), src.Capacity))
	if err != nil {
		return VolInfo{}, err
	}
	defer os.Remove(f)
	var info VolInfo
	err = c.withDirPool(ctx, dstDir, func(pool string) error {
		if _, err := c.run(ctx, "vol-info", "--pool", pool, dstFile); err == nil {
			return fmt.Errorf("%s already exists", dst)
		}
		err := c.withDirPool(ctx, srcDir, func(srcPool string) error {
			_, err := c.run(ctx, "vol-create-from", pool, f, "--inputpool", srcPool, srcFile)
			return err
		})
		if err != nil {
			return err
		}
		info, err = c.volInfo(ctx, pool, dstFile)
		return err
	})
	return info, err
}

// BackedBy lists the VMs with a disk built on the image at path (linked
// clones of a template). It fails rather than guess when it can't tell.
func (c *Client) BackedBy(ctx context.Context, path string) ([]string, error) {
	vms, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	type use struct{ vm, file string }
	byDir := map[string][]use{}
	for _, vm := range vms {
		for _, d := range vm.Disks {
			if d.Device != "disk" || d.Path == "" || strings.HasPrefix(d.Path, "/dev/") {
				continue
			}
			if d.Path == path {
				return []string{vm.Name}, nil // attached directly
			}
			dir, file := splitPath(d.Path)
			byDir[dir] = append(byDir[dir], use{vm.Name, file})
		}
	}
	seen := map[string]bool{}
	var out []string
	for dir, uses := range byDir {
		err := c.withDirPool(ctx, dir, func(pool string) error {
			for _, u := range uses {
				info, err := c.volInfo(ctx, pool, u.file)
				if err != nil {
					continue // a missing disk can't depend on anything
				}
				if info.Backing == path && !seen[u.vm] {
					seen[u.vm] = true
					out = append(out, u.vm)
				}
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("couldn't check the disks in %s: %v", dir, err)
		}
	}
	return out, nil
}

// TemplateDisk is the template a new VM is cloned from.
type TemplateDisk struct {
	ID     string
	Image  VolInfo
	Linked bool
}

// CreateFromTemplate clones the template's disk into the VM's folder, adds
// the cloud-init seed disk (if any) and defines the VM. Anything it made is
// removed again if a later step fails.
func (c *Client) CreateFromTemplate(ctx context.Context, v NewVM, t TemplateDisk, seed []byte) (string, error) {
	caps, err := c.Capabilities(ctx)
	if err != nil {
		return "", err
	}
	folder := strings.TrimRight(firstNonEmpty(v.Disk.Folder, "/mnt/user/domains"), "/")
	if !filepath.IsAbs(folder) {
		return "", errors.New("use a full path for the VM folder")
	}
	v.Template = t.ID
	v.Disk.Mode = "existing"
	v.Disk.Path = filepath.Join(folder, v.Name, "vdisk1.qcow2")
	seedPath := ""
	if seed != nil {
		seedPath = filepath.Join(folder, v.Name, SeedFile)
		v.ISO = seedPath
	}
	xmlText, err := BuildXML(v, caps) // checks the form before any file is made
	if err != nil {
		return "", err
	}
	if _, err := c.run(ctx, "dominfo", "--", v.Name); err == nil {
		return "", fmt.Errorf("a VM called %s already exists", v.Name)
	}
	size := uint64(v.Disk.SizeGB) << 30
	if size < t.Image.Capacity {
		return "", fmt.Errorf("the disk must be at least %d GB, the size of the template", (t.Image.Capacity+(1<<30)-1)>>30)
	}
	if err := c.CloneDisk(ctx, t.Image, v.Disk.Path, size, t.Linked); err != nil {
		return "", fmt.Errorf("couldn't clone the template's disk: %v", err)
	}
	cleanup := func() {
		bg := context.Background()
		_ = c.DeleteFile(bg, v.Disk.Path)
		if seedPath != "" {
			_ = c.DeleteFile(bg, seedPath)
		}
		dir, _ := splitPath(v.Disk.Path)
		c.removeEmptyDir(bg, dir)
	}
	if seed != nil {
		if _, err := c.Upload(ctx, seedPath, bytes.NewReader(seed)); err != nil {
			cleanup()
			return "", fmt.Errorf("couldn't write the cloud-init disk: %v", err)
		}
	}
	if err := c.Define(ctx, xmlText); err != nil {
		cleanup()
		return "", err
	}
	if v.Start {
		if err := c.Action(ctx, v.Name, "start"); err != nil {
			return xmlText, fmt.Errorf("the VM was created but didn't start: %v", err)
		}
	}
	return xmlText, nil
}

// SeedFile is the name of the cloud-init disk in a VM's folder.
const SeedFile = "cloud-init.iso"

// NewMAC returns a random address in QEMU's range (52:54:00).
func NewMAC() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", b[0], b[1], b[2])
}

var reMAC = regexp.MustCompile(`^[0-9a-fA-F]{2}(:[0-9a-fA-F]{2}){5}$`)

func writeTemp(content string) (string, error) {
	f, err := os.CreateTemp("", "homestack-vol-*.xml")
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), f.Close()
}
