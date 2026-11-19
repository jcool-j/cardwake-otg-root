package main

import (
	"archive/zip"
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestCarve(t *testing.T) {
	dir := t.TempDir()
	// build files
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	w, _ := zw.Create("word/document.xml")
	junk := make([]byte, 5000)
	rand.Read(junk)
	w.Write(junk)
	zw.Close()
	pdf := append([]byte("%PDF-1.4\n"), bytes.ReplaceAll(junk[:3000], []byte("%"), []byte("a"))...)
	pdf = append(pdf, []byte("\n%%EOF\n")...)
	// mp4
	var mp4 bytes.Buffer
	box := func(typ string, body []byte) {
		var h [8]byte
		binary.BigEndian.PutUint32(h[:], uint32(8+len(body)))
		copy(h[4:], typ)
		mp4.Write(h[:])
		mp4.Write(body)
	}
	box("ftyp", append([]byte("isom\x00\x00\x00\x00"), []byte("isomiso2")...))
	body := make([]byte, 4000)
	rand.Read(body)
	box("mdat", body)
	box("moov", body[:500])
	// png (minimal valid chunk structure)
	var png bytes.Buffer
	png.WriteString("\x89PNG\r\n\x1a\n")
	chunkw := func(typ string, d []byte) {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(d)))
		png.Write(l[:])
		png.WriteString(typ)
		png.Write(d)
		png.Write([]byte{0, 0, 0, 0})
	}
	chunkw("IHDR", make([]byte, 13))
	chunkw("IDAT", body[:1500])
	chunkw("IEND", nil)
	// jpeg-like: SOI, APP0, SOS then data, EOI
	jpg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 4, 'J', 'F'}
	jpg = append(jpg, 0xFF, 0xDA, 0, 4, 1, 2)
	for i := 0; i < 5000; i++ {
		b := byte(i*7 + 3)
		if b == 0xFF {
			b = 0x10
		}
		jpg = append(jpg, b)
	}
	jpg = append(jpg, 0xFF, 0xD9)
	files := map[string][]byte{"j.jpg": jpg, "p.png": png.Bytes(), "d.docx": zb.Bytes(), "x.pdf": pdf, "v.mp4": mp4.Bytes()}
	img := make([]byte, 2*MB)
	rand.Read(img)
	pos := 512 * 100
	for _, d := range files {
		copy(img[pos:], d)
		pos += (len(d)+511)/512*512 + 512*7
	}
	p := filepath.Join(dir, "card.img")
	os.WriteFile(p, img, 0o644)
	s, err := OpenSource(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	var cancel atomic.Bool
	out := filepath.Join(dir, "out")
	counts, err := Scan(s, out, nil, nil, nil, &cancel)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(counts)
	got := map[[16]byte]bool{}
	filepath.Walk(out, func(q string, i os.FileInfo, e error) error {
		if e == nil && !i.IsDir() {
			b, _ := os.ReadFile(q)
			got[md5.Sum(b)] = true
		}
		return nil
	})
	for n, d := range files {
		if !got[md5.Sum(d)] {
			t.Errorf("%s not recovered", n)
		}
	}
}

func TestFakeCapacity(t *testing.T) {
	// honest file
	dir := t.TempDir()
	p := filepath.Join(dir, "dev.img")
	os.WriteFile(p, make([]byte, 16*MB), 0o644)
	var cancel atomic.Bool
	r, err := FakeCapacity(p, 16*MB, func(Progress) {}, &cancel)
	if err != nil || r.Fake || r.Real != 16*MB {
		t.Fatalf("honest card flagged: %+v %v", r, err)
	}
}

func TestVaultAndShred(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "docs")
	os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	big := make([]byte, 3*MB+17)
	rand.Read(big)
	os.WriteFile(filepath.Join(src, "a.bin"), big, 0o644)
	os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("hello"), 0o644)
	var cancel atomic.Bool
	v := filepath.Join(dir, "x.cwvault")
	if err := VaultEncrypt(src, v, "secret", func(a, b int64) {}, &cancel); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(v)
	if bytes.Contains(raw, []byte("hello")) {
		t.Fatal("plaintext leaked")
	}
	if _, err := VaultDecrypt(v, filepath.Join(dir, "bad"), "wrong", func(a, b int64) {}, &cancel); err == nil {
		t.Fatal("wrong password accepted")
	}
	out := filepath.Join(dir, "out")
	n, err := VaultDecrypt(v, out, "secret", func(a, b int64) {}, &cancel)
	if err != nil || n != 2 {
		t.Fatalf("decrypt: %d %v", n, err)
	}
	b, _ := os.ReadFile(filepath.Join(out, "docs", "a.bin"))
	if !bytes.Equal(b, big) {
		t.Fatal("content mismatch")
	}
	// truncated
	os.WriteFile(v+".cut", raw[:len(raw)-50], 0o644)
	if _, err := VaultDecrypt(v+".cut", filepath.Join(dir, "cut"), "secret", func(a, b int64) {}, &cancel); err == nil {
		t.Fatal("truncated vault accepted")
	}
	// shred
	files, _ := ShredPaths([]string{src}, 2, func(string, bool, error) {}, &cancel)
	if files != 2 {
		t.Fatalf("shredded %d", files)
	}
	if _, err := os.Stat(src); err == nil {
		t.Fatal("folder still exists")
	}
	_ = io.EOF
}

func TestFAT32Mount(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "card.img")
	f, _ := os.Create(p)
	f.Truncate(300 * MB)
	f.Close()
	var cancel atomic.Bool
	if err := FormatFAT32(p, 300*MB, "CWTEST", false, func(Progress) {}, &cancel); err != nil {
		t.Fatal(err)
	}
	mnt := filepath.Join(dir, "mnt")
	os.MkdirAll(mnt, 0o755)
	if out, err := exec.Command("mount", "-t", "vfat", "-o", "loop,offset=1048576", p, mnt).CombinedOutput(); err != nil {
		t.Skipf("cannot loop-mount here: %v %s", err, out)
	}
	defer exec.Command("umount", mnt).Run()
	data := make([]byte, 5*MB)
	rand.Read(data)
	if err := os.WriteFile(filepath.Join(mnt, "t.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(filepath.Join(mnt, "dir"), 0o755)
	b, _ := os.ReadFile(filepath.Join(mnt, "t.bin"))
	if !bytes.Equal(b, data) {
		t.Fatal("readback mismatch")
	}
	out, _ := exec.Command("sh", "-c", "df -k "+mnt+" | tail -1").CombinedOutput()
	t.Logf("df: %s", out)
}

type wrapDev struct {
	data []byte
	real int64
}

func (w *wrapDev) WriteAt(p []byte, off int64) (int, error) {
	for i := range p {
		w.data[(off+int64(i))%w.real] = p[i]
	}
	return len(p), nil
}
func (w *wrapDev) ReadAt(p []byte, off int64) (int, error) {
	for i := range p {
		p[i] = w.data[(off+int64(i))%w.real]
	}
	return len(p), nil
}

type dropDev struct {
	data []byte
	real int64
}

func (w *dropDev) WriteAt(p []byte, off int64) (int, error) {
	for i := range p {
		if off+int64(i) < w.real {
			w.data[off+int64(i)] = p[i]
		}
	}
	return len(p), nil
}
func (w *dropDev) ReadAt(p []byte, off int64) (int, error) {
	for i := range p {
		if off+int64(i) < w.real {
			p[i] = w.data[off+int64(i)]
		} else {
			p[i] = 0
		}
	}
	return len(p), nil
}

func TestFakeDetect(t *testing.T) {
	var cancel atomic.Bool
	noop := func() error { return nil }
	d := &wrapDev{make([]byte, 8*MB), 8 * MB}
	r, _ := fakeCapacityDev(d, noop, 32*MB, func(Progress) {}, &cancel)
	if !r.Fake || r.Real != 8*MB {
		t.Fatalf("wrap fake: %+v", r)
	}
	e := &dropDev{make([]byte, 8*MB), 8 * MB}
	r, _ = fakeCapacityDev(e, noop, 32*MB, func(Progress) {}, &cancel)
	if !r.Fake || r.Real != 8*MB {
		t.Fatalf("drop fake: %+v", r)
	}
}
