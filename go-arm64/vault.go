package main

// Secure shredder and password vault (AES-256-GCM, PBKDF2-SHA256, stdlib only).

import (
	"archive/tar"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
)

// ---------------------------------------------------------------- shredder

// ShredPaths overwrites every file with random data (passes times, last pass
// zeros), truncates, renames and deletes it. Directories are removed afterwards.
func ShredPaths(paths []string, passes int, onFile func(path string, ok bool, err error), cancel *atomic.Bool) (files int, bytes int64) {
	if passes < 1 {
		passes = 1
	}
	var dirs []string
	var list []string
	for _, p := range paths {
		st, err := os.Lstat(p)
		if err != nil {
			onFile(p, false, err)
			continue
		}
		if st.IsDir() {
			filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if d.IsDir() {
					dirs = append(dirs, q)
				} else {
					list = append(list, q)
				}
				return nil
			})
		} else {
			list = append(list, p)
		}
	}
	for _, p := range list {
		if cancel.Load() {
			break
		}
		n, err := shredFile(p, passes, cancel)
		onFile(p, err == nil, err)
		if err == nil {
			files++
			bytes += n
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		os.Remove(d)
	}
	return
}

func shredFile(p string, passes int, cancel *atomic.Bool) (int64, error) {
	st, err := os.Lstat(p)
	if err != nil {
		return 0, err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return 0, os.Remove(p)
	}
	_ = os.Chmod(p, 0o644)
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		return 0, err
	}
	size := st.Size()
	buf := make([]byte, MB)
	for pass := 0; pass < passes; pass++ {
		for off := int64(0); off < size; off += int64(len(buf)) {
			if cancel.Load() {
				f.Close()
				return 0, errors.New("cancelled")
			}
			n := int64(len(buf))
			if off+n > size {
				n = size - off
			}
			if pass == passes-1 {
				for i := range buf[:n] {
					buf[i] = 0
				}
			} else {
				rand.Read(buf[:n])
			}
			if _, err := f.WriteAt(buf[:n], off); err != nil {
				f.Close()
				return 0, err
			}
		}
		f.Sync()
	}
	f.Truncate(0)
	f.Close()
	tmp := filepath.Join(filepath.Dir(p), fmt.Sprintf(".cw%x", randBytes(6)))
	if os.Rename(p, tmp) == nil {
		p = tmp
	}
	return size, os.Remove(p)
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// ---------------------------------------------------------------- vault

const (
	vaultMagic = "CWV1"
	vaultIter  = 200000
	frameSize  = 1 << 20
)

func vaultKey(pw string, salt []byte, iter int) ([]byte, error) {
	return pbkdf2.Key(sha256.New, pw, salt, iter, 32)
}

type frameWriter struct {
	w      io.Writer
	aead   cipher.AEAD
	prefix []byte
	ctr    uint64
	buf    []byte
}

func (fw *frameWriter) nonce() []byte {
	n := make([]byte, 12)
	copy(n, fw.prefix)
	binary.BigEndian.PutUint64(n[4:], fw.ctr)
	return n
}

func (fw *frameWriter) seal(plain []byte, final bool) error {
	aad := []byte{0}
	if final {
		aad[0] = 1
	}
	ct := fw.aead.Seal(nil, fw.nonce(), plain, aad)
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(ct)))
	if _, err := fw.w.Write(l[:]); err != nil {
		return err
	}
	_, err := fw.w.Write(ct)
	fw.ctr++
	return err
}

func (fw *frameWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		room := frameSize - len(fw.buf)
		if room > len(p) {
			room = len(p)
		}
		fw.buf = append(fw.buf, p[:room]...)
		p = p[room:]
		if len(fw.buf) == frameSize {
			if err := fw.seal(fw.buf, false); err != nil {
				return 0, err
			}
			fw.buf = fw.buf[:0]
		}
	}
	return n, nil
}

func (fw *frameWriter) Close() error {
	if len(fw.buf) > 0 {
		if err := fw.seal(fw.buf, false); err != nil {
			return err
		}
	}
	return fw.seal(nil, true)
}

func srcSize(src string) int64 {
	var t int64
	filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if i, e := d.Info(); e == nil {
				t += i.Size()
			}
		}
		return nil
	})
	return t
}

// VaultEncrypt packs a file or folder into a password-protected .cwvault file.
func VaultEncrypt(src, dst, pw string, onProg func(done, total int64), cancel *atomic.Bool) error {
	if pw == "" {
		return errors.New("empty password")
	}
	salt := randBytes(16)
	key, err := vaultKey(pw, salt, vaultIter)
	if err != nil {
		return err
	}
	blk, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(blk)
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	hdr := make([]byte, 0, 28)
	hdr = append(hdr, vaultMagic...)
	hdr = append(hdr, salt...)
	var it [4]byte
	binary.BigEndian.PutUint32(it[:], vaultIter)
	hdr = append(hdr, it[:]...)
	prefix := randBytes(4)
	hdr = append(hdr, prefix...)
	if _, err := out.Write(hdr); err != nil {
		return err
	}
	fw := &frameWriter{w: out, aead: aead, prefix: prefix}
	tw := tar.NewWriter(fw)
	total := srcSize(src)
	var done int64
	root := filepath.Dir(src)
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if cancel.Load() {
			return errors.New("cancelled")
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		info, e := d.Info()
		if e != nil || (!d.IsDir() && !d.Type().IsRegular()) {
			return nil
		}
		h, e := tar.FileInfoHeader(info, "")
		if e != nil {
			return nil
		}
		h.Name = rel
		if d.IsDir() {
			h.Name += "/"
		}
		if e := tw.WriteHeader(h); e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		f, e := os.Open(p)
		if e != nil {
			return e
		}
		defer f.Close()
		buf := make([]byte, 256*1024)
		for {
			n, re := f.Read(buf)
			if n > 0 {
				if _, we := tw.Write(buf[:n]); we != nil {
					return we
				}
				done += int64(n)
				onProg(done, total)
			}
			if re != nil {
				break
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return fw.Close()
}

type frameReader struct {
	r      io.Reader
	aead   cipher.AEAD
	prefix []byte
	ctr    uint64
	buf    []byte
	final  bool
}

func (fr *frameReader) Read(p []byte) (int, error) {
	for len(fr.buf) == 0 {
		if fr.final {
			return 0, io.EOF
		}
		var l [4]byte
		if _, err := io.ReadFull(fr.r, l[:]); err != nil {
			return 0, errors.New("the vault file is truncated or damaged")
		}
		n := binary.BigEndian.Uint32(l[:])
		if n > frameSize+64 {
			return 0, errors.New("the vault file is damaged")
		}
		ct := make([]byte, n)
		if _, err := io.ReadFull(fr.r, ct); err != nil {
			return 0, errors.New("the vault file is truncated or damaged")
		}
		nonce := make([]byte, 12)
		copy(nonce, fr.prefix)
		binary.BigEndian.PutUint64(nonce[4:], fr.ctr)
		pt, err := fr.aead.Open(nil, nonce, ct, []byte{0})
		if err != nil {
			pt, err = fr.aead.Open(nil, nonce, ct, []byte{1})
			if err != nil {
				return 0, errors.New("wrong password, or the file is damaged")
			}
			fr.final = true
		}
		fr.ctr++
		fr.buf = pt
	}
	n := copy(p, fr.buf)
	fr.buf = fr.buf[n:]
	return n, nil
}

// VaultDecrypt unpacks a .cwvault into outDir.
func VaultDecrypt(src, outDir, pw string, onProg func(done, total int64), cancel *atomic.Bool) (int, error) {
	f, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, _ := f.Stat()
	hdr := make([]byte, 28)
	if _, err := io.ReadFull(f, hdr); err != nil || string(hdr[:4]) != vaultMagic {
		return 0, errors.New("this is not a Cardwake vault file")
	}
	iter := int(binary.BigEndian.Uint32(hdr[20:24]))
	if iter < 1000 || iter > 5000000 {
		return 0, errors.New("the vault file is damaged")
	}
	key, err := vaultKey(pw, hdr[4:20], iter)
	if err != nil {
		return 0, err
	}
	blk, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(blk)
	fr := &frameReader{r: f, aead: aead, prefix: hdr[24:28]}
	tr := tar.NewReader(fr)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return 0, err
	}
	absOut, _ := filepath.Abs(outDir)
	count := 0
	var done int64
	for {
		if cancel.Load() {
			return count, errors.New("cancelled")
		}
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return count, err
		}
		name := filepath.FromSlash(strings.TrimSuffix(h.Name, "/"))
		target := filepath.Join(absOut, name)
		if !strings.HasPrefix(target, absOut+string(os.PathSeparator)) && target != absOut {
			return count, errors.New("unsafe path inside the vault")
		}
		if h.Typeflag == tar.TypeDir {
			os.MkdirAll(target, 0o755)
			continue
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		os.MkdirAll(filepath.Dir(target), 0o755)
		o, err := os.Create(target)
		if err != nil {
			return count, err
		}
		n, err := io.Copy(o, tr)
		o.Close()
		if err != nil {
			return count, err
		}
		done += n
		count++
		onProg(done, st.Size())
	}
	return count, nil
}

var _ = bytes.MinRead
