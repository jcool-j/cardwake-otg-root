package main

// Cardwake engine: raw device access (bad-sector tolerant), file carving,
// imaging, restore, surface scan, fake-capacity test and benchmark.

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	mrand "math/rand"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"
)

const (
	sector = 512
	MB     = 1024 * 1024
	chunk  = 8 * MB
)

var groups = map[string][]string{
	"photos":   {"jpg", "png", "gif", "bmp", "webp", "heic"},
	"videos":   {"mp4", "mov", "3gp", "avi"},
	"audio":    {"wav", "m4a"},
	"docs":     {"pdf", "docx", "xlsx", "pptx", "epub"},
	"archives": {"zip", "7z", "apk"},
}

// ---------------------------------------------------------------- Source

type Range struct{ Off, Len int64 }

type Source struct {
	f      *os.File
	Size   int64
	Bad    int64 // unreadable sectors
	Ranges []Range
}

func OpenSource(path string, hint int64) (*Source, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	s := &Source{f: f}
	sz, err := deviceSize(f)
	if err != nil || sz <= 0 {
		if hint > 0 {
			sz = hint
		} else if err == nil {
			err = fmt.Errorf("cannot determine size")
		}
		if sz <= 0 {
			f.Close()
			return nil, err
		}
	}
	s.Size = sz
	return s, nil
}

func (s *Source) Close() { s.f.Close() }

func (s *Source) markBad(off, n int64) {
	s.Bad += n / sector
	if k := len(s.Ranges); k > 0 && s.Ranges[k-1].Off+s.Ranges[k-1].Len == off {
		s.Ranges[k-1].Len += n
	} else if k < 100000 {
		s.Ranges = append(s.Ranges, Range{off, n})
	}
}

// readAligned reads [a, a+n) (both multiples of the sector size); unreadable
// areas are zero-filled and recorded. A run of failures skips ahead fast.
func (s *Source) readAligned(a int64, n int) []byte {
	buf := make([]byte, n)
	if got, err := s.f.ReadAt(buf, a); err == nil || (got == n) {
		return buf
	}
	const blk = 64 * 1024
	for p := 0; p < n; p += blk {
		l := blk
		if p+l > n {
			l = n - p
		}
		if got, err := s.f.ReadAt(buf[p:p+l], a+int64(p)); err == nil || got == l {
			continue
		}
		fails := 0
		for q := 0; q < l; q += sector {
			if fails >= 16 { // dead zone: skip the rest of this block
				s.markBad(a+int64(p+q), int64(l-q))
				for i := p + q; i < p+l; i++ {
					buf[i] = 0
				}
				break
			}
			got, err := s.f.ReadAt(buf[p+q:p+q+sector], a+int64(p+q))
			if err != nil && got < sector {
				fails++
				for i := p + q; i < p+q+sector; i++ {
					buf[i] = 0
				}
				s.markBad(a+int64(p+q), sector)
			} else {
				fails = 0
			}
		}
	}
	return buf
}

func (s *Source) Read(off int64, n int) []byte {
	if off < 0 || off >= s.Size || n <= 0 {
		return nil
	}
	if off+int64(n) > s.Size {
		n = int(s.Size - off)
	}
	a := off - off%sector
	end := off + int64(n)
	e := end + (sector-end%sector)%sector
	if e > s.Size+sector {
		e = s.Size
	}
	d := s.readAligned(a, int(e-a))
	st := int(off - a)
	if st+n > len(d) {
		n = len(d) - st
	}
	return d[st : st+n]
}

// ---------------------------------------------------------------- carvers

type carved struct {
	length int64
	ext    string
	ok     bool
}
type carver func(s *Source, off int64) *carved

func be32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }
func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }

func carveJPG(s *Source, off int64) *carved {
	d := s.Read(off, 32*MB)
	n := len(d)
	if n < 4 || d[2] != 0xFF || d[3] < 0xC0 || d[3] > 0xFE {
		return nil
	}
	pos := 2
	for pos+4 <= n {
		if d[pos] != 0xFF {
			return nil
		}
		m := d[pos+1]
		if m == 0xFF {
			pos++
			continue
		}
		if m == 0xD9 {
			return &carved{int64(pos + 2), "jpg", true}
		}
		if m == 0x00 || m == 0x01 || m == 0xD8 || (m >= 0xD0 && m <= 0xD7) {
			pos += 2
			continue
		}
		sl := int(d[pos+2])<<8 | int(d[pos+3])
		if sl < 2 {
			return nil
		}
		pos += 2 + sl
		if m == 0xDA {
			for {
				i := bytes.IndexByte(d[min(pos, n):], 0xFF)
				if i < 0 || pos+i+1 >= n {
					return &carved{int64(n), "jpg", false}
				}
				i += pos
				nb := d[i+1]
				if nb == 0xFF {
					pos = i + 1
				} else if nb == 0 || (nb >= 0xD0 && nb <= 0xD7) {
					pos = i + 2
				} else {
					pos = i
					break
				}
			}
		}
	}
	return &carved{int64(n), "jpg", false}
}

func carvePNG(s *Source, off int64) *carved {
	pos := int64(8)
	for pos < 100*MB {
		h := s.Read(off+pos, 8)
		if len(h) < 8 {
			return nil
		}
		ln := int64(be32(h))
		typ := h[4:8]
		for _, c := range typ {
			if !(c >= 65 && c <= 90 || c >= 97 && c <= 122) {
				return nil
			}
		}
		if pos == 8 && string(typ) != "IHDR" {
			return nil
		}
		if ln > 0x7FFFFFFF {
			return nil
		}
		pos += 12 + ln
		if string(typ) == "IEND" {
			return &carved{pos, "png", true}
		}
	}
	return nil
}

func skipSub(d []byte, pos int) int {
	for pos < len(d) {
		ln := int(d[pos])
		pos += 1 + ln
		if ln == 0 {
			return pos
		}
	}
	return -1
}

func carveGIF(s *Source, off int64) *carved {
	d := s.Read(off, 30*MB)
	n := len(d)
	if n < 14 {
		return nil
	}
	f := d[10]
	pos := 13
	if f&0x80 != 0 {
		pos += 3 * (1 << ((f & 7) + 1))
	}
	for pos >= 0 && pos < n {
		switch d[pos] {
		case 0x3B:
			return &carved{int64(pos + 1), "gif", true}
		case 0x21:
			pos = skipSub(d, pos+2)
		case 0x2C:
			if pos+10 > n {
				return nil
			}
			lf := d[pos+9]
			pos += 10
			if lf&0x80 != 0 {
				pos += 3 * (1 << ((lf & 7) + 1))
			}
			pos = skipSub(d, pos+1)
		default:
			return nil
		}
	}
	return nil
}

func carvePDF(s *Source, off int64) *carved {
	d := s.Read(off, 100*MB)
	limit := len(d)
	if nx := bytes.Index(d[5:], []byte("%PDF-")); nx >= 0 {
		limit = nx + 5
	}
	e := bytes.LastIndex(d[:limit], []byte("%%EOF"))
	if e < 0 {
		return nil
	}
	end := e + 5
	for end < len(d) && end < e+7 && (d[end] == 10 || d[end] == 13) {
		end++
	}
	return &carved{int64(end), "pdf", true}
}

func carveZIP(s *Source, off int64) *carved {
	head := s.Read(off, 160)
	if len(head) < 30 {
		return nil
	}
	var pos int64
	for pos < 500*MB && off+pos < s.Size {
		blk := s.Read(off+pos, 4*MB+4)
		if len(blk) == 0 {
			break
		}
		base := 0
		for {
			i := bytes.Index(blk[base:], []byte("PK\x05\x06"))
			if i < 0 {
				break
			}
			i += base
			p := pos + int64(i)
			e := s.Read(off+p, 22)
			if len(e) == 22 {
				cdsize := int64(le32(e[12:]))
				cdoff := int64(le32(e[16:]))
				clen := int64(binary.LittleEndian.Uint16(e[20:]))
				if cdoff+cdsize == p {
					cd := s.Read(off+cdoff, int(min64(cdsize, 4*MB)))
					ext := "zip"
					switch {
					case bytes.Contains(cd, []byte("AndroidManifest.xml")):
						ext = "apk"
					case bytes.Contains(cd, []byte("word/")):
						ext = "docx"
					case bytes.Contains(cd, []byte("xl/")):
						ext = "xlsx"
					case bytes.Contains(cd, []byte("ppt/")):
						ext = "pptx"
					case bytes.Contains(head, []byte("application/epub+zip")):
						ext = "epub"
					}
					return &carved{p + 22 + clen, ext, true}
				}
			}
			base = i + 1
		}
		if len(blk) < 4*MB {
			break
		}
		pos += 4 * MB
	}
	return nil
}

var mp4Boxes = map[string]bool{"ftyp": true, "moov": true, "mdat": true, "free": true, "skip": true, "wide": true, "uuid": true,
	"moof": true, "mfra": true, "pnot": true, "udta": true, "meta": true, "styp": true, "sidx": true, "junk": true, "prft": true, "pdin": true}

func carveMP4(s *Source, off int64) *carved {
	h := s.Read(off, 16)
	if len(h) < 12 || string(h[4:8]) != "ftyp" {
		return nil
	}
	brand := string(h[8:12])
	var pos int64
	boxes := 0
	for {
		b := s.Read(off+pos, 16)
		if len(b) < 8 {
			break
		}
		size := int64(be32(b))
		typ := string(b[4:8])
		if !mp4Boxes[typ] {
			break
		}
		if size == 1 {
			if len(b) < 16 {
				break
			}
			size = int64(binary.BigEndian.Uint64(b[8:16]))
		} else if size == 0 {
			break
		}
		if size < 8 || off+pos+size > s.Size+sector {
			if boxes == 0 {
				return nil
			}
			break
		}
		pos += size
		boxes++
	}
	if pos < 1024 {
		return nil
	}
	ext := "mp4"
	switch {
	case brand == "heic" || brand == "heix" || brand == "mif1" || brand == "msf1":
		ext = "heic"
	case brand == "qt  ":
		ext = "mov"
	case brand[:3] == "3gp":
		ext = "3gp"
	case brand == "M4A " || brand == "M4B ":
		ext = "m4a"
	}
	return &carved{pos, ext, true}
}

func carveRIFF(s *Source, off int64) *carved {
	h := s.Read(off, 12)
	if len(h) < 12 {
		return nil
	}
	size := int64(le32(h[4:]))
	ext := map[string]string{"WAVE": "wav", "AVI ": "avi", "WEBP": "webp"}[string(h[8:12])]
	if ext == "" || size < 1000 {
		return nil
	}
	return &carved{min64(size+8, s.Size-off), ext, true}
}

func carveBMP(s *Source, off int64) *carved {
	h := s.Read(off, 30)
	if len(h) < 30 {
		return nil
	}
	size := int64(le32(h[2:]))
	res := le32(h[6:])
	pix := int64(le32(h[10:]))
	dib := le32(h[14:])
	switch dib {
	case 12, 40, 52, 56, 64, 108, 124:
	default:
		return nil
	}
	if res != 0 || size < 1024 || size > 100*MB || pix >= size {
		return nil
	}
	return &carved{size, "bmp", true}
}

func carve7z(s *Source, off int64) *carved {
	h := s.Read(off, 32)
	if len(h) < 32 {
		return nil
	}
	noff := int64(binary.LittleEndian.Uint64(h[12:]))
	nsize := int64(binary.LittleEndian.Uint64(h[20:]))
	total := 32 + noff + nsize
	if total < 64 || off+total > s.Size || noff < 0 || nsize < 0 {
		return nil
	}
	return &carved{total, "7z", true}
}

type sig struct {
	pat   []byte
	delta int
	fn    carver
}

var sigs = []sig{
	{[]byte{0xff, 0xd8, 0xff}, 0, carveJPG},
	{[]byte("\x89PNG\r\n\x1a\n"), 0, carvePNG},
	{[]byte("GIF87a"), 0, carveGIF},
	{[]byte("GIF89a"), 0, carveGIF},
	{[]byte("%PDF-"), 0, carvePDF},
	{[]byte("PK\x03\x04"), 0, carveZIP},
	{[]byte("ftyp"), -4, carveMP4},
	{[]byte("RIFF"), 0, carveRIFF},
	{[]byte("BM"), 0, carveBMP},
	{[]byte("7z\xbc\xaf\x27\x1c"), 0, carve7z},
}
var minSize = map[string]int64{"jpg": 2048, "png": 200, "gif": 100, "bmp": 1024}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------- scan

type Progress struct {
	Done, Total int64
	Found       int
	Counts      map[string]int
	Speed       float64
	Bad         int64
}

type FileInfo struct {
	Rel  string `json:"rel"`
	Ext  string `json:"ext"`
	Size int64  `json:"size"`
	OK   bool   `json:"ok"`
}

func Scan(s *Source, outdir string, allowed map[string]bool, onProg func(Progress), onFile func(FileInfo), cancel *atomic.Bool) (map[string]int, error) {
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		return nil, err
	}
	counts := map[string]int{}
	total := 0
	var skipUntil int64
	t0 := time.Now()
	for off := int64(0); off < s.Size; off += chunk {
		if cancel.Load() {
			break
		}
		cb := s.Read(off, chunk)
		type hit struct {
			start int
			fn    carver
		}
		var hits []hit
		for _, sg := range sigs {
			base := 0
			for {
				i := bytes.Index(cb[base:], sg.pat)
				if i < 0 {
					break
				}
				i += base
				st := i + sg.delta
				if st >= 0 && (off+int64(st))%sector == 0 {
					hits = append(hits, hit{st, sg.fn})
				}
				base = i + 1
			}
		}
		sort.SliceStable(hits, func(i, j int) bool { return hits[i].start < hits[j].start })
		for _, h := range hits {
			if cancel.Load() {
				break
			}
			a := off + int64(h.start)
			if a < skipUntil {
				continue
			}
			r := h.fn(s, a)
			if r == nil {
				continue
			}
			m := minSize[r.ext]
			if m == 0 {
				m = 512
			}
			if r.length < m {
				continue
			}
			skipUntil = a + r.length
			if allowed != nil && !allowed[r.ext] {
				continue
			}
			counts[r.ext]++
			total++
			sub := filepath.Join(outdir, r.ext)
			if err := os.MkdirAll(sub, 0o755); err != nil {
				return counts, err
			}
			suffix := ""
			if !r.ok {
				suffix = "_partial"
			}
			name := fmt.Sprintf("%s_%06d%s.%s", r.ext, counts[r.ext], suffix, r.ext)
			out, err := os.Create(filepath.Join(sub, name))
			if err != nil {
				return counts, err
			}
			for p := int64(0); p < r.length; p += 4 * MB {
				step := int(min64(4*MB, r.length-p))
				if _, err := out.Write(s.Read(a+p, step)); err != nil {
					out.Close()
					return counts, err
				}
			}
			out.Close()
			if onFile != nil {
				onFile(FileInfo{r.ext + "/" + name, r.ext, r.length, r.ok})
			}
		}
		if onProg != nil {
			done := min64(off+chunk, s.Size)
			cp := map[string]int{}
			for k, v := range counts {
				cp[k] = v
			}
			onProg(Progress{done, s.Size, total, cp, float64(done) / maxf(time.Since(t0).Seconds(), 0.01), s.Bad})
		}
	}
	return counts, nil
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// MakeImage copies the whole source into a file (bad sectors zero-filled).
func MakeImage(s *Source, outpath string, onProg func(Progress), cancel *atomic.Bool) error {
	// O_EXCL: never overwrite an existing file (it may be an earlier backup)
	out, err := os.OpenFile(outpath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("the file already exists - choose a new name (nothing was overwritten): %s", outpath)
		}
		return err
	}
	defer out.Close()
	t0 := time.Now()
	for pos := int64(0); pos < s.Size; pos += chunk {
		if cancel.Load() {
			break
		}
		n := int(min64(chunk, s.Size-pos))
		if _, err := out.Write(s.Read(pos, n)); err != nil {
			return err
		}
		if onProg != nil {
			done := pos + int64(n)
			onProg(Progress{Done: done, Total: s.Size, Speed: float64(done) / maxf(time.Since(t0).Seconds(), 0.01), Bad: s.Bad})
		}
	}
	return nil
}

// RestoreImage writes an image file onto a raw target and optionally verifies.
func RestoreImage(imgPath, target string, targetSize int64, verify bool, onProg func(Progress), cancel *atomic.Bool) (string, error) {
	img, err := os.Open(imgPath)
	if err != nil {
		return "", err
	}
	defer img.Close()
	st, _ := img.Stat()
	isz := st.Size()
	if targetSize > 0 && isz > targetSize {
		return "", fmt.Errorf("the image (%d bytes) is larger than the card (%d bytes)", isz, targetSize)
	}
	dst, err := os.OpenFile(target, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	defer dst.Close()
	total := isz
	if verify {
		total *= 2
	}
	t0 := time.Now()
	buf := make([]byte, 4*MB)
	var done int64
	for pos := int64(0); pos < isz; {
		if cancel.Load() {
			return "cancelled", nil
		}
		n, rerr := img.ReadAt(buf, pos)
		if n == 0 && rerr != nil {
			break
		}
		wn := n
		if wn%sector != 0 {
			wn += sector - wn%sector
			for i := n; i < wn; i++ {
				buf[i] = 0
			}
		}
		if _, err := dst.WriteAt(buf[:wn], pos); err != nil {
			return "", fmt.Errorf("write failed at %d: %w", pos, err)
		}
		pos += int64(n)
		done += int64(n)
		onProg(Progress{Done: done, Total: total, Speed: float64(done) / maxf(time.Since(t0).Seconds(), 0.01)})
	}
	dst.Sync()
	if !verify {
		return "ok", nil
	}
	b2 := make([]byte, 4*MB)
	for pos := int64(0); pos < isz; {
		if cancel.Load() {
			return "cancelled", nil
		}
		n, _ := img.ReadAt(buf, pos)
		if n == 0 {
			break
		}
		rn := n
		if rn%sector != 0 {
			rn += sector - rn%sector
		}
		if _, err := dst.ReadAt(b2[:rn], pos); err != nil {
			return "", fmt.Errorf("verify read failed at %d: %w", pos, err)
		}
		if !bytes.Equal(buf[:n], b2[:n]) {
			return "mismatch", fmt.Errorf("verify mismatch near byte %d", pos)
		}
		pos += int64(n)
		done += int64(n)
		onProg(Progress{Done: done, Total: total, Speed: float64(done) / maxf(time.Since(t0).Seconds(), 0.01)})
	}
	return "ok", nil
}

// ---------------------------------------------------------------- surface scan

const mapCells = 1024

type SurfaceResult struct {
	Cells    []byte  `json:"cells"` // 0 = ok, 1 = slow, 2 = bad
	BadBytes int64   `json:"badBytes"`
	Slow     int     `json:"slow"`
	Bad      int     `json:"bad"`
	AvgMBs   float64 `json:"avgMBs"`
}

// SurfaceScan reads the whole device sequentially and maps ok / slow / bad cells.
func SurfaceScan(s *Source, cells []byte, onProg func(Progress), cancel *atomic.Bool) SurfaceResult {
	per := s.Size/int64(len(cells)) + 1
	per += (sector - per%sector) % sector
	var res SurfaceResult
	const blk = 1 * MB
	t0 := time.Now()
	var lat []float64
	for off := int64(0); off < s.Size; off += blk {
		if cancel.Load() {
			break
		}
		n := int(min64(blk, s.Size-off))
		before := s.Bad
		ts := time.Now()
		s.Read(off, n)
		d := time.Since(ts).Seconds()
		lat = append(lat, d)
		ci := int(off / per)
		if ci >= len(cells) {
			ci = len(cells) - 1
		}
		if s.Bad > before {
			cells[ci] = 2
		} else if d > 0.35 && cells[ci] == 0 {
			cells[ci] = 1
		}
		onProg(Progress{Done: off + int64(n), Total: s.Size, Speed: float64(off+int64(n)) / maxf(time.Since(t0).Seconds(), 0.01), Bad: s.Bad})
	}
	for _, c := range cells {
		if c == 2 {
			res.Bad++
		} else if c == 1 {
			res.Slow++
		}
	}
	res.Cells = append([]byte(nil), cells...)
	res.BadBytes = s.Bad * sector
	res.AvgMBs = float64(s.Size) / MB / maxf(time.Since(t0).Seconds(), 0.001)
	return res
}

// ---------------------------------------------------------------- fake capacity

type FakeResult struct {
	Claimed  int64  `json:"claimed"`
	Real     int64  `json:"real"`
	Fake     bool   `json:"fake"`
	Mismatch int    `json:"mismatch"`
	Verdict  string `json:"verdict"`
}

// patternBlock fills b with a deterministic pseudo-random pattern bound to the block index.
func patternBlock(idx int64, b []byte) {
	r := mrand.New(mrand.NewSource(idx*2654435761 + 0x9E3779B9))
	r.Read(b)
	binary.LittleEndian.PutUint64(b[0:8], uint64(idx)) // unique stamp
}

// FakeCapacity writes a unique block into every 1 MB slot, then reads everything
// back. Cards that wrap around / drop data are exposed. DESTROYS data on target.
func FakeCapacity(target string, size int64, onProg func(Progress), cancel *atomic.Bool) (FakeResult, error) {
	f, err := os.OpenFile(target, os.O_RDWR, 0)
	if err != nil {
		return FakeResult{}, err
	}
	defer f.Close()
	return fakeCapacityDev(f, f.Sync, size, onProg, cancel)
}

type rwAt interface {
	io.ReaderAt
	io.WriterAt
}

func fakeCapacityDev(f rwAt, sync func() error, size int64, onProg func(Progress), cancel *atomic.Bool) (FakeResult, error) {
	claimed := size
	nblk := size / MB
	if nblk == 0 {
		return FakeResult{}, fmt.Errorf("the card size is unknown or too small to test")
	}
	writeFailed := int64(-1)
	buf := make([]byte, MB)
	rd := make([]byte, MB)
	t0 := time.Now()
	for i := int64(0); i < nblk; i++ {
		if cancel.Load() {
			return FakeResult{}, fmt.Errorf("cancelled")
		}
		patternBlock(i, buf)
		if _, err := f.WriteAt(buf, i*MB); err != nil {
			// a write error: either the card is write-protected (i == 0) or the medium really ends here
			writeFailed = i
			nblk = i
			size = i * MB
			break
		}
		onProg(Progress{Done: i * MB, Total: size * 2, Speed: float64(i*MB) / maxf(time.Since(t0).Seconds(), 0.01)})
	}
	sync()
	if writeFailed == 0 {
		// the very first write was refused: nothing was tested, so never report a capacity verdict
		return FakeResult{Claimed: claimed, Verdict: "writeerror"}, nil
	}
	var good, bad int64
	firstBad := int64(-1)
	for i := int64(0); i < nblk; i++ {
		if cancel.Load() {
			return FakeResult{}, fmt.Errorf("cancelled")
		}
		if _, err := f.ReadAt(rd, i*MB); err != nil {
			bad++
			if firstBad < 0 {
				firstBad = i
			}
			continue
		}
		patternBlock(i, buf)
		if bytes.Equal(buf, rd) {
			good++
		} else {
			bad++
			if firstBad < 0 {
				firstBad = i
			}
		}
		onProg(Progress{Done: size + i*MB, Total: size * 2, Speed: float64(size+i*MB) / maxf(time.Since(t0).Seconds(), 0.01)})
	}
	_ = firstBad
	res := FakeResult{Claimed: claimed, Real: good * MB, Mismatch: int(bad)}
	if bad > 0 || writeFailed > 0 {
		res.Fake = true
		res.Verdict = "fake"
	} else {
		res.Verdict = "ok"
	}
	return res, nil
}

// ---------------------------------------------------------------- benchmark

type BenchResult struct {
	SeqRead   float64 `json:"seqRead"`   // MB/s
	SeqWrite  float64 `json:"seqWrite"`  // MB/s
	RandRead  float64 `json:"randRead"`  // 4K IOPS
	RandWrite float64 `json:"randWrite"` // 4K IOPS
	Class     string  `json:"class"`
	V         string  `json:"v"`
	A         string  `json:"a"`
}

// Benchmark: raw sequential/random reads from the device and write tests on a temp file inside dir (a folder on the card).
func Benchmark(devPath string, devSize int64, writeDir string, onStep func(string, float64), cancel *atomic.Bool) (BenchResult, error) {
	var r BenchResult
	if devPath != "" {
		f, err := os.Open(devPath)
		if err != nil {
			return r, err
		}
		sz := devSize
		span := int64(256 * MB)
		if sz < span {
			span = sz - sz%sector
		}
		buf := make([]byte, 4*MB)
		t0 := time.Now()
		var done int64
		for done+int64(len(buf)) <= span {
			if _, err := f.ReadAt(buf, done); err != nil {
				break
			}
			done += int64(len(buf))
			if cancel.Load() {
				break
			}
		}
		r.SeqRead = float64(done) / MB / maxf(time.Since(t0).Seconds(), 0.001)
		onStep("seqRead", r.SeqRead)
		small := make([]byte, 4096)
		rng := mrand.New(mrand.NewSource(7))
		n := 0
		t0 = time.Now()
		for time.Since(t0) < 3*time.Second && !cancel.Load() {
			off := (rng.Int63n(sz-4096) / 4096) * 4096
			if _, err := f.ReadAt(small, off); err != nil {
				break
			}
			n++
		}
		r.RandRead = float64(n) / maxf(time.Since(t0).Seconds(), 0.001)
		onStep("randRead", r.RandRead)
		f.Close()
	}
	if writeDir != "" {
		tmp := filepath.Join(writeDir, "__cardwake_bench.tmp")
		defer os.Remove(tmp)
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
		if err != nil {
			return r, err
		}
		defer f.Close()
		buf := make([]byte, 4*MB)
		rand.Read(buf)
		total := 64 * MB
		t0 := time.Now()
		for w := 0; w < total; w += len(buf) {
			if _, err := f.Write(buf); err != nil {
				return r, err
			}
		}
		f.Sync()
		r.SeqWrite = float64(total) / MB / maxf(time.Since(t0).Seconds(), 0.001)
		onStep("seqWrite", r.SeqWrite)
		small := make([]byte, 4096)
		rand.Read(small)
		rng := mrand.New(mrand.NewSource(9))
		n := 0
		t0 = time.Now()
		for time.Since(t0) < 3*time.Second && !cancel.Load() {
			off := (rng.Int63n(int64(total)-4096) / 4096) * 4096
			if _, err := f.WriteAt(small, off); err != nil {
				break
			}
			n++
			if n%8 == 0 {
				f.Sync()
			}
		}
		f.Sync()
		r.RandWrite = float64(n) / maxf(time.Since(t0).Seconds(), 0.001)
		onStep("randWrite", r.RandWrite)
	}
	// ratings (SD Association minimum sustained write speeds)
	w := r.SeqWrite
	switch {
	case w >= 90:
		r.V = "V90"
	case w >= 60:
		r.V = "V60"
	case w >= 30:
		r.V = "V30 / U3"
	case w >= 10:
		r.V = "V10 / U1"
	case w >= 6:
		r.V = "V6"
	case w > 0:
		r.V = "-"
	}
	switch {
	case w >= 10:
		r.Class = "Class 10"
	case w >= 6:
		r.Class = "Class 6"
	case w >= 4:
		r.Class = "Class 4"
	case w >= 2:
		r.Class = "Class 2"
	}
	if r.RandRead >= 1500 && r.RandWrite >= 500 {
		r.A = "A1"
		if r.RandRead >= 4000 && r.RandWrite >= 2000 {
			r.A = "A2"
		}
	}
	return r, nil
}
