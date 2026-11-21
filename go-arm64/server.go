package main

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

//go:embed ui/index.html
var uiFS embed.FS

const (
	appName    = "Cardwake"
	appVersion = "1.1.1"
)

var (
	token      string
	lastPing   atomic.Int64
	everPinged atomic.Bool
	byeAt      atomic.Int64
)

func newToken() string {
	b := make([]byte, 18)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	w.Write(b)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "127.0.0.1" && host != "localhost" {
			fail(w, 403, "forbidden")
			return
		}
		t := r.Header.Get("X-Token")
		if t == "" {
			t = r.URL.Query().Get("t")
		}
		if subtle.ConstantTimeCompare([]byte(t), []byte(token)) != 1 {
			fail(w, 403, "forbidden")
			return
		}
		h(w, r)
	}
}

type req map[string]any

func body(r *http.Request) req {
	var m req
	json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&m)
	if m == nil {
		m = req{}
	}
	return m
}

func (m req) str(k string) string {
	if v, ok := m[k].(string); ok {
		return strings.TrimSpace(strings.Trim(strings.TrimSpace(v), `"`))
	}
	if f, ok := m[k].(float64); ok {
		return strconv.FormatInt(int64(f), 10)
	}
	return ""
}
func (m req) num(k string) int64 {
	switch v := m[k].(type) {
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n
	}
	return -1
}
func (m req) flag(k string) bool { v, _ := m[k].(bool); return v }
func (m req) list(k string) []string {
	var out []string
	if a, ok := m[k].([]any); ok {
		for _, x := range a {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func diskArg(m req) (Disk, error) {
	n := m.num("disk")
	if n < 0 {
		return Disk{}, errors.New("no disk selected")
	}
	d, ok := findDisk(int(n))
	if !ok {
		return Disk{}, errors.New("Disk not found (was the card removed?)")
	}
	return d, nil
}

func confirmed(m req, d Disk) error {
	if m.str("confirm") != strconv.Itoa(d.Number) {
		return errors.New("confirmation does not match the disk number")
	}
	return nil
}

func jobResp(w http.ResponseWriter, j *Job, err error) {
	if errors.Is(err, errBusy) {
		fail(w, 409, "busy")
		return
	}
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"job": j.ID})
}

func cleanLabel(l string, max int) string {
	var b strings.Builder
	for _, c := range strings.ToUpper(l) {
		if c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			b.WriteRune(c)
		}
	}
	s := b.String()
	if len(s) > max {
		s = s[:max]
	}
	if s == "" {
		s = "CARD"
	}
	return s
}

func buildMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", authed(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			fail(w, 404, "not found")
			return
		}
		b, _ := uiFS.ReadFile("ui/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	}))
	mux.HandleFunc("/api/info", authed(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"app": appName, "version": appVersion, "admin": isAdmin(), "win": isWindows, "demo": !isWindows})
	}))
	mux.HandleFunc("/api/disks", authed(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"disks": listDisks()})
	}))
	mux.HandleFunc("/api/state", authed(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		since, _ := strconv.Atoi(q.Get("since"))
		ls, _ := strconv.Atoi(q.Get("logsince"))
		jobMu.Lock()
		j := curJob
		jobMu.Unlock()
		if j == nil {
			writeJSON(w, 200, map[string]any{"job": nil})
			return
		}
		if q.Get("job") != j.ID {
			since, ls = 0, 0
		}
		writeJSON(w, 200, map[string]any{"job": j.snapshot(since, ls)})
	}))
	mux.HandleFunc("/api/thumb", authed(func(w http.ResponseWriter, r *http.Request) {
		jobMu.Lock()
		j := curJob
		jobMu.Unlock()
		if j == nil || j.Outdir == "" {
			fail(w, 404, "none")
			return
		}
		base, _ := filepath.EvalSymlinks(j.Outdir)
		full, err := filepath.EvalSymlinks(filepath.Join(base, filepath.FromSlash(r.URL.Query().Get("f"))))
		if err != nil || !strings.HasPrefix(full, base+string(os.PathSeparator)) {
			fail(w, 404, "none")
			return
		}
		ct := mime.TypeByExtension(filepath.Ext(full))
		if !strings.HasPrefix(ct, "image/") {
			fail(w, 415, "not an image")
			return
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "max-age=3600")
		http.ServeFile(w, r, full)
	}))
	mux.HandleFunc("/api/devinfo", authed(func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("disk"))
		d, ok := findDisk(n)
		if !ok {
			fail(w, 404, "Disk not found")
			return
		}
		if !isWindows {
			writeJSON(w, 200, map[string]any{"model": d.Name, "manufacturer": "Generic", "interface": "USB", "firmware": "1.00", "serial": "DEMO0001",
				"mediaType": "Removable Media", "size": d.Size, "bytesPerSector": 512, "vid": "058F", "pid": "6387", "physSector": 512, "logSector": 512,
				"partStyle": d.Style, "health": d.Health, "opStatus": d.OpStatus, "readonly": d.ReadOnly, "pnp": `USBSTOR\DISK&VEN_GENERIC&PROD_MASS_STORAGE&REV_1.00\DEMO&0`})
			return
		}
		out, _ := psRun(psDevInfo, 40*time.Second, "-n", strconv.Itoa(n))
		var m map[string]any
		if i := strings.Index(out, "{"); i >= 0 {
			json.Unmarshal([]byte(out[i:]), &m)
		}
		if m == nil {
			fail(w, 500, "could not read device information")
			return
		}
		writeJSON(w, 200, m)
	}))
	mux.HandleFunc("/api/parts", authed(func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("disk"))
		d, ok := findDisk(n)
		if !ok {
			fail(w, 404, "Disk not found")
			return
		}
		if !isWindows {
			var parts []map[string]any
			for i, v := range d.Volumes {
				parts = append(parts, map[string]any{"number": i + 1, "letter": v.Letter, "size": v.Size, "offset": 1048576, "type": "Fat32", "fs": v.FS, "label": v.Label, "free": v.Free})
			}
			writeJSON(w, 200, map[string]any{"parts": parts, "freeExtent": 1 * gb})
			return
		}
		out, _ := psRun(psParts, 40*time.Second, "-n", strconv.Itoa(n))
		var m map[string]any
		if i := strings.Index(out, "{"); i >= 0 {
			json.Unmarshal([]byte(out[i:]), &m)
		}
		if m == nil {
			m = map[string]any{"parts": []any{}, "freeExtent": 0}
		}
		writeJSON(w, 200, m)
	}))

	post := func(path string, h func(w http.ResponseWriter, r *http.Request, m req)) {
		mux.HandleFunc(path, authed(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				fail(w, 405, "POST only")
				return
			}
			h(w, r, body(r))
		}))
	}
	post("/api/ping", func(w http.ResponseWriter, r *http.Request, m req) {
		lastPing.Store(time.Now().Unix())
		everPinged.Store(true)
		byeAt.Store(0)
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	post("/api/bye", func(w http.ResponseWriter, r *http.Request, m req) {
		byeAt.Store(time.Now().Unix() + 4)
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	post("/api/pick", func(w http.ResponseWriter, r *http.Request, m req) {
		kind := m.str("kind")
		if !isWindows {
			p := filepath.Join(os.TempDir(), "cardwake_demo_out")
			if kind != "folder" {
				p = demoAny
				if kind == "image" {
					p = demoDev
				}
			}
			writeJSON(w, 200, map[string]any{"path": p, "paths": []string{p}})
			return
		}
		args := []string{"-kind", "file"}
		switch kind {
		case "folder":
			args = []string{"-kind", "folder"}
		case "image":
			args = append(args, "-filter", "Disk images (*.img;*.dd;*.raw;*.bin)|*.img;*.dd;*.raw;*.bin|All files (*.*)|*.*")
		case "vault":
			args = append(args, "-filter", "Cardwake vault (*.cwvault)|*.cwvault|All files (*.*)|*.*")
		case "any":
			args = append(args, "-multi", "1")
		}
		out, _ := psRun(psPick, 10*time.Minute, args...)
		var paths []string
		for _, l := range strings.Split(out, "\n") {
			if t := strings.TrimSpace(l); t != "" {
				paths = append(paths, t)
			}
		}
		p := ""
		if len(paths) > 0 {
			p = paths[0]
		}
		writeJSON(w, 200, map[string]any{"path": p, "paths": paths})
	})
	post("/api/open", func(w http.ResponseWriter, r *http.Request, m req) {
		jobMu.Lock()
		j := curJob
		jobMu.Unlock()
		target := ""
		if j != nil && j.Outdir != "" {
			target = j.Outdir
			if rel := m.str("rel"); rel != "" {
				base, _ := filepath.EvalSymlinks(j.Outdir)
				full, err := filepath.EvalSymlinks(filepath.Join(base, filepath.FromSlash(rel)))
				if err == nil && strings.HasPrefix(full, base+string(os.PathSeparator)) {
					target = full
				}
			}
		} else if p := m.str("path"); p != "" {
			// only ever open existing folders - never launch an arbitrary file
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				target = p
			}
		}
		if target != "" {
			openPath(target)
		}
		writeJSON(w, 200, map[string]bool{"ok": target != ""})
	})
	post("/api/cancel", func(w http.ResponseWriter, r *http.Request, m req) {
		jobMu.Lock()
		if curJob != nil {
			curJob.Cancel.Store(true)
		}
		jobMu.Unlock()
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	post("/api/dismiss", func(w http.ResponseWriter, r *http.Request, m req) {
		jobMu.Lock()
		if curJob != nil && curJob.Status != "running" {
			curJob = nil
		}
		jobMu.Unlock()
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	post("/api/unlock", func(w http.ResponseWriter, r *http.Request, m req) {
		d, err := diskArg(m)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		j, err := startJob("unlock", d.Name, []string{"disk", "partitions", "diskpart", "policy", "verify"}, false, func(j *Job) error { return jobUnlock(j, d.Number) })
		jobResp(w, j, err)
	})
	post("/api/chkdsk", func(w http.ResponseWriter, r *http.Request, m req) {
		d, err := diskArg(m)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		if len(d.Volumes) == 0 {
			fail(w, 400, "This card has no recognised file system (RAW). Recover files first, then format.")
			return
		}
		l := d.Volumes[0].Letter
		j, err := startJob("chkdsk", d.Name, []string{"chkdsk"}, false, func(j *Job) error { return jobChkdsk(j, l) })
		jobResp(w, j, err)
	})
	post("/api/format", func(w http.ResponseWriter, r *http.Request, m req) {
		d, err := diskArg(m)
		if err == nil {
			err = confirmed(m, d)
		}
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		fs := m.str("fs")
		if fs != "FAT32" && fs != "exFAT" && fs != "NTFS" {
			fs = "exFAT"
		}
		label := cleanLabel(m.str("label"), 11)
		full := m.str("mode") == "full"
		steps, ring := []string{"clean", "partition", "format"}, false
		if fs == "FAT32" && d.Size > 32*1024*1024*1024 {
			steps, ring = []string{"clean", "format"}, true
		}
		j, err := startJob("format", d.Name, steps, ring, func(j *Job) error { return jobFormat(j, d, fs, label, full) })
		jobResp(w, j, err)
	})
	post("/api/recover", func(w http.ResponseWriter, r *http.Request, m req) {
		out := m.str("outdir")
		if out == "" {
			fail(w, 400, "Choose an output folder.")
			return
		}
		types := m.list("types")
		if img := m.str("image"); img != "" {
			if st, err := os.Stat(img); err != nil || st.IsDir() {
				fail(w, 400, "Image file not found.")
				return
			}
			j, err := startJob("recover", filepath.Base(img), nil, true, func(j *Job) error { return jobRecover(j, nil, img, out, types) })
			jobResp(w, j, err)
			return
		}
		d, err := diskArg(m)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		j, err := startJob("recover", d.Name, nil, true, func(j *Job) error { return jobRecover(j, &d, devPath(d.Number), out, types) })
		jobResp(w, j, err)
	})
	post("/api/image", func(w http.ResponseWriter, r *http.Request, m req) {
		d, err := diskArg(m)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		out := m.str("outfile")
		if out == "" {
			fail(w, 400, "Choose where to save the image.")
			return
		}
		j, err := startJob("image", d.Name, nil, true, func(j *Job) error { return jobImage(j, d, out) })
		jobResp(w, j, err)
	})
	post("/api/restore", func(w http.ResponseWriter, r *http.Request, m req) {
		d, err := diskArg(m)
		if err == nil {
			err = confirmed(m, d)
		}
		img := m.str("image")
		if err == nil {
			if st, e := os.Stat(img); e != nil || st.IsDir() {
				err = errors.New("Image file not found.")
			} else if st.Size() > realSize(d) {
				err = fmt.Errorf("the image (%s) is larger than the card (%s)", fmtBytes(st.Size()), fmtBytes(realSize(d)))
			}
		}
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		verify := m.flag("verify")
		steps := []string{"clean", "write"}
		if verify {
			steps = append(steps, "verify")
		}
		j, err := startJob("restore", d.Name, steps, true, func(j *Job) error { return jobRestore(j, d, img, verify) })
		jobResp(w, j, err)
	})
	post("/api/diagnose", func(w http.ResponseWriter, r *http.Request, m req) {
		d, err := diskArg(m)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		j, err := startJob("diagnose", d.Name, []string{"size", "read", "write", "verdict"}, false, func(j *Job) error { return jobDiagnose(j, d) })
		jobResp(w, j, err)
	})
	post("/api/surface", func(w http.ResponseWriter, r *http.Request, m req) {
		d, err := diskArg(m)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		j, err := startJob("surface", d.Name, nil, true, func(j *Job) error { return jobSurface(j, d) })
		jobResp(w, j, err)
	})
	post("/api/fakecap", func(w http.ResponseWriter, r *http.Request, m req) {
		d, err := diskArg(m)
		if err == nil {
			err = confirmed(m, d)
		}
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		j, err := startJob("fakecap", d.Name, []string{"clean", "test"}, true, func(j *Job) error { return jobFake(j, d) })
		jobResp(w, j, err)
	})
	post("/api/bench", func(w http.ResponseWriter, r *http.Request, m req) {
		d, err := diskArg(m)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		j, err := startJob("bench", d.Name, nil, false, func(j *Job) error { return jobBench(j, d) })
		jobResp(w, j, err)
	})
	post("/api/wblock", func(w http.ResponseWriter, r *http.Request, m req) {
		d, err := diskArg(m)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		on := "0"
		if m.flag("on") {
			on = "1"
		}
		j, err := startJob("wblock", d.Name, []string{"apply", "verify"}, false, func(j *Job) error {
			j.set("on", on == "1")
			return jobPS(j, psWBlock, nil, "-n", itoa(d.Number), "-on", on)
		})
		jobResp(w, j, err)
	})
	post("/api/eject", func(w http.ResponseWriter, r *http.Request, m req) {
		d, err := diskArg(m)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		letter := ""
		if len(d.Volumes) > 0 {
			letter = d.Volumes[0].Letter
		}
		kill := "0"
		if m.flag("kill") {
			kill = "1"
		}
		j, err := startJob("eject", d.Name, []string{"flush", "release", "eject"}, false, func(j *Job) error {
			return jobPS(j, psEject, nil, "-letter", letter, "-kill", kill, "-n", itoa(d.Number))
		})
		jobResp(w, j, err)
	})
	post("/api/bitlocker", func(w http.ResponseWriter, r *http.Request, m req) {
		d, err := diskArg(m)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		pw, _ := m["password"].(string)
		if len(d.Volumes) == 0 || pw == "" {
			fail(w, 400, "The card needs a drive letter and a password.")
			return
		}
		l := d.Volumes[0].Letter
		j, err := startJob("bitlocker", d.Name, []string{"encrypt"}, false, func(j *Job) error {
			return jobPS(j, psBitlocker, []string{"CW_PW=" + pw}, "-letter", l)
		})
		jobResp(w, j, err)
	})
	post("/api/shred", func(w http.ResponseWriter, r *http.Request, m req) {
		paths := m.list("paths")
		if len(paths) == 0 {
			fail(w, 400, "Choose files or a folder to shred.")
			return
		}
		passes := int(m.num("passes"))
		if passes < 1 || passes > 7 {
			passes = 1
		}
		for _, p := range paths {
			if len(filepath.Clean(p)) <= 3 {
				fail(w, 400, "Refusing to shred an entire drive.")
				return
			}
		}
		j, err := startJob("shred", fmt.Sprintf("%d item(s)", len(paths)), nil, false, func(j *Job) error { return jobShred(j, paths, passes) })
		jobResp(w, j, err)
	})
	post("/api/vault", func(w http.ResponseWriter, r *http.Request, m req) {
		mode, path, out := m.str("mode"), m.str("path"), m.str("out")
		pw, _ := m["password"].(string)
		if pw == "" || path == "" || out == "" || (mode != "enc" && mode != "dec") {
			fail(w, 400, "Choose a source, a destination and a password.")
			return
		}
		if _, err := os.Stat(path); err != nil {
			fail(w, 400, "Source not found.")
			return
		}
		j, err := startJob("vault", filepath.Base(path), nil, true, func(j *Job) error { return jobVault(j, mode, path, out, pw) })
		jobResp(w, j, err)
	})
	post("/api/partop", func(w http.ResponseWriter, r *http.Request, m req) {
		d, err := diskArg(m)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		op := m.str("op")
		if op == "delete" || op == "create" || op == "resize" {
			if err := confirmed(m, d); err != nil {
				fail(w, 400, err.Error())
				return
			}
		}
		if !isWindows {
			writeJSON(w, 200, map[string]any{"ok": true, "log": []string{"(demo) " + op + " done"}})
			return
		}
		fs := m.str("fs")
		if fs != "FAT32" && fs != "exFAT" && fs != "NTFS" {
			fs = "exFAT"
		}
		args := []string{"-n", itoa(d.Number), "-op", op, "-part", strconv.FormatInt(max(m.num("part"), 0), 10),
			"-letter", strings.ToUpper(m.str("letter")), "-newletter", strings.ToUpper(m.str("newletter")),
			"-label", cleanLabel(m.str("label"), 32), "-fs", fs, "-size", strconv.FormatInt(max(m.num("size"), 0), 10)}
		out, _ := psRun(psPartOp, 5*time.Minute, args...)
		ok := strings.Contains(out, "OK|") && !strings.Contains(out, "FAIL|")
		var lines []string
		for _, l := range strings.Split(out, "\n") {
			if t := strings.TrimSpace(l); t != "" {
				lines = append(lines, t)
			}
		}
		writeJSON(w, 200, map[string]any{"ok": ok, "log": lines})
	})
	return mux
}
