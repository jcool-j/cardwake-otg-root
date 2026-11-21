package main

// Own FAT32 formatter: lets Cardwake format cards above 32 GB as FAT32
// (Windows refuses). Writes an MBR + one FAT32 partition to a RAW disk that
// has no partitions/volumes (the caller clears the disk first).

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
)

func zeroRange(f *os.File, off, n int64, cancel *atomic.Bool, tick func(int64)) error {
	z := make([]byte, 4*MB)
	for p := int64(0); p < n; p += int64(len(z)) {
		if cancel != nil && cancel.Load() {
			return fmt.Errorf("cancelled")
		}
		l := int64(len(z))
		if p+l > n {
			l = n - p
		}
		if _, err := f.WriteAt(z[:l], off+p); err != nil {
			return fmt.Errorf("write failed at %d: %w", off+p, err)
		}
		if tick != nil {
			tick(p + l)
		}
	}
	return nil
}

func fatLabel(l string) []byte {
	l = strings.ToUpper(l)
	b := []byte("           ")
	copy(b, l)
	return b
}

// FormatFAT32 formats target (size bytes). full=true zero-fills the whole device first.
func FormatFAT32(target string, size int64, label string, full bool, onProg func(Progress), cancel *atomic.Bool) error {
	f, err := os.OpenFile(target, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	total := size / sector
	const start = 2048
	vol := total - start
	if vol > 0xFFFFFFFF {
		vol = 0xFFFFFFFF
	}
	volBytes := vol * sector
	var spc int64
	switch {
	case volBytes <= 260*MB:
		spc = 1
	case volBytes <= 8*1024*MB:
		spc = 8
	case volBytes <= 16*1024*MB:
		spc = 16
	case volBytes <= 32*1024*MB:
		spc = 32
	default:
		spc = 64
	}
	const reserved = 32
	tmp1 := vol - reserved
	tmp2 := (256*spc + 2) / 2
	fatSz := (tmp1 + tmp2 - 1) / tmp2
	clusters := (vol - reserved - 2*fatSz) / spc
	if clusters < 65525 {
		return fmt.Errorf("card too small for FAT32 (%d clusters)", clusters)
	}
	prog := func(done int64) {
		if onProg != nil {
			onProg(Progress{Done: done, Total: size})
		}
	}
	if full {
		if err := zeroRange(f, 0, size-size%sector, cancel, prog); err != nil {
			return err
		}
	}
	// wipe MBR gap + reserved + FATs
	dataStart := int64(start) + reserved + 2*fatSz
	if err := zeroRange(f, 0, (dataStart+spc)*sector, cancel, nil); err != nil {
		return err
	}
	// MBR
	mbr := make([]byte, 512)
	rand.Read(mbr[440:444])
	e := mbr[446:462]
	e[0] = 0x00
	e[1], e[2], e[3] = 0xFE, 0xFF, 0xFF
	e[4] = 0x0C
	e[5], e[6], e[7] = 0xFE, 0xFF, 0xFF
	binary.LittleEndian.PutUint32(e[8:], start)
	binary.LittleEndian.PutUint32(e[12:], uint32(vol))
	mbr[510], mbr[511] = 0x55, 0xAA
	if _, err := f.WriteAt(mbr, 0); err != nil {
		return err
	}
	// boot sector
	bs := make([]byte, 512)
	copy(bs[0:], []byte{0xEB, 0x58, 0x90})
	copy(bs[3:], "MSWIN4.1")
	binary.LittleEndian.PutUint16(bs[11:], 512)
	bs[13] = byte(spc)
	binary.LittleEndian.PutUint16(bs[14:], reserved)
	bs[16] = 2
	bs[21] = 0xF8
	binary.LittleEndian.PutUint16(bs[24:], 63)
	binary.LittleEndian.PutUint16(bs[26:], 255)
	binary.LittleEndian.PutUint32(bs[28:], start)
	binary.LittleEndian.PutUint32(bs[32:], uint32(vol))
	binary.LittleEndian.PutUint32(bs[36:], uint32(fatSz))
	binary.LittleEndian.PutUint32(bs[44:], 2)
	binary.LittleEndian.PutUint16(bs[48:], 1)
	binary.LittleEndian.PutUint16(bs[50:], 6)
	bs[64] = 0x80
	bs[66] = 0x29
	rand.Read(bs[67:71])
	copy(bs[71:82], fatLabel(label))
	copy(bs[82:], "FAT32   ")
	bs[510], bs[511] = 0x55, 0xAA
	fs := make([]byte, 512)
	binary.LittleEndian.PutUint32(fs[0:], 0x41615252)
	binary.LittleEndian.PutUint32(fs[484:], 0x61417272)
	binary.LittleEndian.PutUint32(fs[488:], uint32(clusters-1))
	binary.LittleEndian.PutUint32(fs[492:], 3)
	fs[510], fs[511] = 0x55, 0xAA
	s2 := make([]byte, 512)
	s2[510], s2[511] = 0x55, 0xAA
	base := int64(start) * sector
	for i, sec := range [][]byte{bs, fs, s2} {
		if _, err := f.WriteAt(sec, base+int64(i)*sector); err != nil {
			return err
		}
		if _, err := f.WriteAt(sec, base+int64(6+i)*sector); err != nil { // backup copies
			return err
		}
	}
	// FATs
	fat := make([]byte, 512)
	binary.LittleEndian.PutUint32(fat[0:], 0x0FFFFFF8)
	binary.LittleEndian.PutUint32(fat[4:], 0x0FFFFFFF)
	binary.LittleEndian.PutUint32(fat[8:], 0x0FFFFFFF)
	fat1 := base + reserved*sector
	if _, err := f.WriteAt(fat, fat1); err != nil {
		return err
	}
	if _, err := f.WriteAt(fat, fat1+fatSz*sector); err != nil {
		return err
	}
	// root directory: volume label entry
	root := make([]byte, 512)
	copy(root[0:11], fatLabel(label))
	root[11] = 0x08
	if _, err := f.WriteAt(root, dataStart*sector); err != nil {
		return err
	}
	return f.Sync()
}
