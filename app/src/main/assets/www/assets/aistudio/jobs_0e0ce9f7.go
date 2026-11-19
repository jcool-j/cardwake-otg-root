package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------- disks

type Volume struct {
	Letter string `json:"letter"`
	FS     string `json:"fs"`
	Label  string `json:"label"`
	Size   int64  `json:"size"`
	Free   int64  `json:"free"`
	RO     bool   `json:"ro"`
}

type Disk struct {
	Number     int      `json:"number"`
	Name       string   `json:"name"`
	Size       int64    `json:"size"`
	Bus        string   `json:"bus"`
	Style      string   `json:"style"`
	ReadOnly   bool     `json:"readonly"`
	Offline    bool     `json:"offline"`
	Partitions int      `json:"partitions"`
	Volumes    []Volume `json:"volumes"`
	Health     string   `json:"health"`
	OpStatus   string   `json:"opstatus"`
	Removable  bool     `json:"removable"`
}

var (
	demoDev string // device file used for everything in demo mode
	demoAny string // path returned by pickers in demo mode
)

const gb = 1000 * 1000 * 1000

func demoDisks() []Disk {
	return []Disk{
		{Number: 1, Name: "SanDisk Ultra SD Card", Size: 32 * gb, Bus: "SD", Style: "MBR", ReadOnly: true, Partitions: 1, Health: "Healthy", OpStatus: "Online",
			Volumes: []Volume{{"E", "FAT32", "CANON", 31 * gb, 12 * gb, true}}},
		{Number: 2, Name: "Kingston DataTraveler", Size: 16 * gb, Bus: "USB", Style: "RAW", Health: "Healthy", OpStatus: "Online"},
		{Number: 3, Name: "Generic MMC Reader", Size: 64 * gb, Bus: "SD", Style: "MBR", Partitions: 1, Health: "Healthy", OpStatus: "Online",
			Volumes: []Volume{{"F", "exFAT", "GOPRO", 63 * gb, 40 * gb, false}}},
		{Number: 4, Name: "Generic SD Reader", Size: 0, Bus: "SD", Style: "RAW", Health: "Healthy", OpStatus: "No Media"},
	}
}

func listDisks() []Disk {
	var disks []Disk
	if !isWindows {
		disks = demoDisks()
	} else {
		out, err := psRun(psList, 60*time.Second)
		if err == nil || out != "" {
			if i := strings.Index(out, "["); i >= 0 {
				json.Unmarshal([]byte(out[i:]), &disks)
			}
		}
	}
	for i := range disks {
		switch strings.ToUpper(disks[i].Bus) {
		case "USB", "SD", "MMC", "SDIO", "UNKNOWN", "":
			disks[i].Removable = true
		}
		if disks[i].Volumes == nil {
			disks[i].Volumes = []Volume{}
		}
	}
	return disks
}

func findDisk(n int) (Disk, bool) {
	for _, d := range listDisks() {
		if d.Number == n {
			return d, true
		}
	}
	return Disk{}, false
}

func devPath(n int) string {
	if !isWindows {
		return demoDev
	}
	return fmt.Sprintf(`\\.\PhysicalDrive%d`, n)
}

func realSize(d Disk) int64 {
	if !isWindows {
		if st, err := os.Stat(demoDev); err == nil {
			return st.Size()
		}
	}
	return d.Size
}

func letters(d Disk) []string {
	var l []string
	for _, v := range d.Volumes {
		l = append(l, strings.ToUpper(v.Letter))
	}
	return l
}

func onSameDisk(path string, d Disk) bool {
	if len(path) >= 2 && path[1] == ':' {
		for _, l := range letters(d) {
			if strings.EqualFold(path[:1], l) {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------- jobs

type LogLine struct {
	T   float64 `json:"t"`
	Lvl string  `json:"lvl"`
	Msg string  `json:"msg"`
}

type Job struct {
	mu      sync.Mutex
	ID      string
	Kind    string
	Title   string
	Status  string
	Started time.Time
	Ended   time.Time
	Log     []LogLine
	Files   []FileInfo
	Steps   []string
	Step    int
	Ring    bool
	Prog    Progress
	Result  map[string]any
	Error   string
	Outdir  string
	Cells   string
	Cancel  atomic.Bool
}

type Snapshot struct {
	ID       string         `json:"id"`
	Kind     string         `json:"kind"`
	Title    string         `json:"title"`
	Status   string         `json:"status"`
	Elapsed  float64        `json:"elapsed"`
	Steps    []string       `json:"steps"`
	Step     int            `json:"step"`
	Ring     bool           `json:"ring"`
	Progress map[string]any `json:"progress"`
	Result   map[string]any `json:"result"`
	Error    string         `json:"error"`
	Outdir   string         `json:"outdir"`
	NFiles   int            `json:"nfiles"`
	Files    []FileInfo     `json:"files"`
	Log      []LogLine      `json:"log"`
	Cells    string         `json:"cells"`
}

var (
	jobMu  sync.Mutex
	curJob *Job
)

var errBusy = errors.New("busy")

func (j *Job) say(lvl, msg string) {
	j.mu.Lock()
	j.Log = append(j.Log, LogLine{round1(time.Since(j.Started).Seconds()), lvl, msg})
	j.mu.Unlock()
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

func (j *Job) set(k string, v any) {
	j.mu.Lock()
	j.Result[k] = v
	j.mu.Unlock()
}

func (j *Job) setStep(i int) {
	j.mu.Lock()
	j.Step = i
	j.mu.Unlock()
}

func (j *Job) progress(p Progress) {
	j.mu.Lock()
	j.Prog = p
	j.mu.Unlock()
}

func (j *Job) handleLine(line string) {
	if line == "" {
		return
	}
	tag, rest, ok := strings.Cut(line, "|")
	if !ok {
		j.say("info", line)
		return
	}
	switch tag {
	case "STEP":
		for i, s := range j.Steps {
			if s == rest {
				j.setStep(i)
			}
		}
	case "RESULT":
		j.set("state", rest)
	case "OK", "WARN", "FAIL", "INFO":
		j.say(strings.ToLower(tag), rest)
	default:
		j.say("info", line)
	}
}

func (j *Job) snapshot(since, logSince int) Snapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	end := j.Ended
	if end.IsZero() {
		end = time.Now()
	}
	s := Snapshot{ID: j.ID, Kind: j.Kind, Title: j.Title, Status: j.Status, Elapsed: round1(end.Sub(j.Started).Seconds()),
		Steps: j.Steps, Step: j.Step, Ring: j.Ring, Error: j.Error, Outdir: j.Outdir, NFiles: len(j.Files), Cells: j.Cells,
		Progress: map[string]any{"done": j.Prog.Done, "total": j.Prog.Total, "found": j.Prog.Found, "counts": j.Prog.Counts, "speed": j.Prog.Speed, "bad": j.Prog.Bad},
	}
	s.Result = map[string]any{}
	for k, v := range j.Result {
		s.Result[k] = v
	}
	if since < 0 || since > len(j.Files) {
		since = 0
	}
	e := min(len(j.Files), since+400)
	s.Files = append([]FileInfo{}, j.Files[since:e]...)
	if logSince < 0 || logSince > len(j.Log) {
		logSince = 0
	}
	e = min(len(j.Log), logSince+400)
	s.Log = append([]LogLine{}, j.Log[logSince:e]...)
	return s
}

func startJob(kind, title string, steps []string, ring bool, fn func(j *Job) error) (*Job, error) {
	jobMu.Lock()
	if curJob != nil && curJob.Status == "running" {
		jobMu.Unlock()
		return nil, errBusy
	}
	j := &Job{ID: fmt.Sprintf("%x", time.Now().UnixNano()&0xffffffff), Kind: kind, Title: title, Status: "running", Started: time.Now(),
		Steps: steps, Step: -1, Ring: ring, Result: map[string]any{}, Prog: Progress{Counts: map[string]int{}}}
	curJob = j
	jobMu.Unlock()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				j.Error = fmt.Sprint("internal error: ", r)
				j.say("fail", j.Error)
				j.mu.Lock()
				j.Status = "error"
				j.mu.Unlock()
			}
			j.mu.Lock()
			j.Ended = time.Now()
			j.mu.Unlock()
		}()
		err := fn(j)
		j.mu.Lock()
		defer j.mu.Unlock()
		switch {
		case err != nil && j.Cancel.Load():
			j.Status = "cancelled"
		case err != nil:
			j.Error = err.Error()
			j.Log = append(j.Log, LogLine{round1(time.Since(j.Started).Seconds()), "fail", err.Error()})
			j.Status = "error"
		case j.Cancel.Load():
			j.Status = "cancelled"
		default:
			j.Status = "done"
		}
	}()
	return j, nil
}

// ---------------------------------------------------------------- job bodies

func itoa(i int) string { return strconv.Itoa(i) }

func prepareRaw(j *Job, n int) error {
	if !isWindows {
		j.setStep(0)
		j.say("ok", "(demo) disk wiped")
		return nil
	}
	if err := psStream(j, psClear, nil, "-n", itoa(n)); err != nil {
		return fmt.Errorf("could not release the disk for raw access: %w", err)
	}
	if st, _ := j.Result["state"].(string); st != "ok" {
		return errors.New("could not wipe the partition table (is the card locked?)")
	}
	return nil
}

func rescan(j *Job, n int) {
	if !isWindows {
		return
	}
	psStream(j, psRescan, nil, "-n", itoa(n))
}

func jobUnlock(j *Job, n int) error {
	if !isWindows {
		for i, l := range []struct{ lvl, msg string }{{"info", "IsReadOnly=True  IsOffline=False"}, {"ok", "Disk read-only flag cleared"}, {"ok", "Partition 1 unlocked"},
			{"info", "DiskPart successfully cleared the disk attributes."}, {"ok", "WriteProtect policy: not set"}, {"fail", "Write test failed on E:"}} {
			time.Sleep(450 * time.Millisecond)
			j.setStep([]int{0, 0, 1, 2, 3, 4}[i])
			j.say(l.lvl, l.msg)
		}
		j.setStep(len(j.Steps))
		j.set("state", "locked")
		return nil
	}
	err := psStream(j, psUnlock, nil, "-n", itoa(n))
	j.setStep(len(j.Steps))
	if _, ok := j.Result["state"]; !ok {
		j.set("state", "failed")
	}
	return err
}

func jobChkdsk(j *Job, letter string) error {
	j.setStep(0)
	if !isWindows {
		time.Sleep(600 * time.Millisecond)
		j.say("info", "The type of the file system is FAT32.")
		j.say("info", "Windows has scanned the file system and found no problems.")
		j.setStep(1)
		j.set("state", "ok")
		return nil
	}
	out, err := psRun("cmd /c \"echo Y|chkdsk "+letter+": /f /x\"", 30*time.Minute)
	for _, l := range strings.Split(out, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			j.say("info", t)
		}
	}
	j.setStep(1)
	state := "ok"
	if err != nil {
		state = "problems"
	}
	if strings.Contains(strings.ToLower(out), "made corrections") || strings.Contains(strings.ToLower(out), "fixed") {
		state = "fixed"
	}
	j.set("state", state)
	return nil
}

func jobFormat(j *Job, d Disk, fs, label string, full bool) error {
	own := fs == "FAT32" && d.Size > 32*1024*1024*1024
	if own {
		j.Steps = []string{"clean", "format"}
		j.Ring = true
		j.say("info", "FAT32 above 32 GB: Cardwake uses its own formatter (Windows refuses this).")
		if err := prepareRaw(j, d.Number); err != nil {
			j.set("state", "failed")
			return err
		}
		j.setStep(1)
		j.say("info", map[bool]string{true: "Full format: zero-filling the whole card...", false: "Quick format..."}[full])
		err := FormatFAT32(devPath(d.Number), realSize(d), label, full, j.progress, &j.Cancel)
		if err != nil {
			j.set("state", "failed")
			return err
		}
		rescan(j, d.Number)
		j.say("ok", "Formatted as FAT32")
		j.setStep(len(j.Steps))
		j.set("state", "ok")
		return nil
	}
	if full {
		j.Ring = false
	}
	if !isWindows {
		for i, m := range []string{"Disk wiped", "Partition created, drive G:", "Formatted G: as " + fs} {
			time.Sleep(500 * time.Millisecond)
			j.setStep(i)
			j.say("ok", m)
		}
		j.setStep(3)
		j.set("state", "ok")
		return nil
	}
	f := "0"
	if full {
		f = "1"
	}
	err := psStream(j, psFormat, nil, "-n", itoa(d.Number), "-fs", fs, "-label", label, "-full", f)
	j.setStep(len(j.Steps))
	if _, ok := j.Result["state"]; !ok {
		j.set("state", "failed")
	}
	return err
}

func jobRecover(j *Job, d *Disk, source string, outbase string, types []string) error {
	var hint int64
	var lt []string
	if d != nil {
		hint = realSize(*d)
		lt = letters(*d)
	}
	if d != nil && onSameDisk(outbase, *d) {
		return errors.New("the output folder is on the card itself - choose a folder on another drive")
	}
	outdir := filepath.Join(outbase, "Cardwake_Recovered_"+time.Now().Format("20060102_150405"))
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		return err
	}
	j.Outdir = outdir
	_ = lt
	s, err := OpenSource(source, hint)
	if err != nil {
		return err
	}
	defer s.Close()
	j.say("info", "Source: "+source)
	allowed := map[string]bool{}
	for _, t := range types {
		for _, e := range groups[t] {
			allowed[e] = true
		}
	}
	if len(allowed) == 0 {
		allowed = nil
	}
	counts, err := Scan(s, outdir, allowed, j.progress, func(f FileInfo) {
		j.mu.Lock()
		j.Files = append(j.Files, f)
		j.mu.Unlock()
	}, &j.Cancel)
	n := 0
	for _, c := range counts {
		n += c
	}
	j.set("state", "ok")
	j.set("found", n)
	j.set("bad", s.Bad)
	if err != nil {
		return err
	}
	j.say("ok", fmt.Sprintf("Finished: %d files", n))
	if n == 0 {
		j.say("warn", "Nothing found. If the card was written to after formatting, or TRIM wiped it, the data may be gone.")
	}
	return nil
}

func jobImage(j *Job, d Disk, outfile string) error {
	if onSameDisk(outfile, d) {
		return errors.New("the image file is on the card itself - choose another drive")
	}
	s, err := OpenSource(devPath(d.Number), realSize(d))
	if err != nil {
		return err
	}
	defer s.Close()
	j.Prog.Total = s.Size
	j.Outdir = filepath.Dir(outfile)
	err = MakeImage(s, outfile, j.progress, &j.Cancel)
	if err != nil {
		return err
	}
	j.set("state", "ok")
	j.set("file", outfile)
	j.set("bad", s.Bad)
	j.set("badRanges", len(s.Ranges))
	j.say("ok", "Image saved: "+outfile)
	if s.Bad > 0 {
		j.say("warn", fmt.Sprintf("%d unreadable sectors were skipped (filled with zeros).", s.Bad))
	}
	return nil
}

func jobRestore(j *Job, d Disk, img string, verify bool) error {
	if err := prepareRaw(j, d.Number); err != nil {
		return err
	}
	j.setStep(1)
	j.say("info", "Writing "+img)
	j.mu.Lock()
	j.Prog.Total = 1
	j.mu.Unlock()
	st, err := RestoreImage(img, devPath(d.Number), realSize(d), verify, func(p Progress) {
		j.progress(p)
		if verify && p.Done > p.Total/2 {
			j.setStep(2)
		}
	}, &j.Cancel)
	if err != nil {
		j.set("state", map[bool]string{true: "mismatch", false: "failed"}[st == "mismatch"])
		return err
	}
	if st == "cancelled" {
		return errors.New("cancelled")
	}
	j.setStep(len(j.Steps))
	rescan(j, d.Number)
	j.set("state", "ok")
	j.set("verified", verify)
	j.say("ok", "Image written to the card")
	return nil
}

func jobDiagnose(j *Job, d Disk) error {
	res := map[string]any{"claimed": d.Size, "opstatus": d.OpStatus, "health": d.Health}
	ro := d.ReadOnly
	for _, v := range d.Volumes {
		ro = ro || v.RO
	}
	res["readonlyFlag"] = ro
	finish := func(state string) error {
		j.setStep(4)
		for k, v := range res {
			j.set(k, v)
		}
		j.set("state", state)
		return nil
	}
	j.setStep(0)
	j.say("info", "Querying the physical drive size...")
	if d.Size == 0 || strings.Contains(strings.ToLower(d.OpStatus), "no media") {
		j.say("fail", "Windows reports no media / 0 bytes")
		res["phys"] = 0
		return finish("dead")
	}
	f, err := os.Open(devPath(d.Number))
	if err != nil {
		j.say("fail", "Cannot open the physical drive: "+err.Error())
		res["openError"] = err.Error()
		res["phys"] = 0
		return finish("dead")
	}
	phys, _ := deviceSize(f)
	res["phys"] = phys
	j.say("info", "Physical size: "+fmtBytes(phys))
	if phys == 0 {
		f.Close()
		return finish("dead")
	}
	j.setStep(1)
	okR, total := 0, 0
	buf := make([]byte, 4096)
	offs := []int64{0, 4096, 8192, 65536, 1 * MB}
	for i := int64(1); i <= 16; i++ {
		offs = append(offs, (phys/17)*i-(phys/17*i)%4096)
	}
	offs = append(offs, phys-phys%4096-4096)
	for _, o := range offs {
		if o < 0 || o+4096 > phys {
			continue
		}
		total++
		if _, err := f.ReadAt(buf, o); err == nil {
			okR++
		}
	}
	f.Close()
	res["readsOK"], res["readsTotal"] = okR, total
	j.say(map[bool]string{true: "ok", false: "fail"}[okR == total], fmt.Sprintf("Read test: %d of %d sample reads succeeded", okR, total))
	j.setStep(2)
	wt := "na"
	if !isWindows {
		if !ro {
			wt = "ok"
		} else {
			wt = "fail"
		}
	} else {
		for _, v := range d.Volumes {
			p := v.Letter + `:\__cardwake_w.tmp`
			if err := os.WriteFile(p, []byte("cardwake"), 0o644); err != nil {
				wt = "fail"
				j.say("fail", "Write test failed on "+v.Letter+":")
				continue
			}
			if err := os.Remove(p); err != nil {
				wt = "fail"
			} else if wt != "fail" {
				wt = "ok"
				j.say("ok", "Write test passed on "+v.Letter+": (file created and deleted)")
			}
		}
	}
	res["writeTest"] = wt
	j.setStep(3)
	switch {
	case total > 0 && okR == 0:
		return finish("dead")
	case ro:
		return finish("swlock")
	case wt == "fail":
		return finish("hwlock")
	case okR < total:
		return finish("degraded")
	}
	return finish("ok")
}

func fmtBytes(n int64) string {
	u := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(n)
	i := 0
	for f >= 1000 && i < 4 {
		f /= 1000
		i++
	}
	return fmt.Sprintf("%.1f %s", f, u[i])
}

func jobSurface(j *Job, d Disk) error {
	s, err := OpenSource(devPath(d.Number), realSize(d))
	if err != nil {
		return err
	}
	defer s.Close()
	cells := make([]byte, mapCells)
	res := SurfaceScan(s, cells, func(p Progress) {
		b := make([]byte, len(cells))
		for i, c := range cells {
			b[i] = '0' + c
		}
		j.mu.Lock()
		j.Prog = p
		j.Cells = string(b)
		j.mu.Unlock()
	}, &j.Cancel)
	b := make([]byte, len(res.Cells))
	for i, c := range res.Cells {
		b[i] = '0' + c
	}
	j.mu.Lock()
	j.Cells = string(b)
	j.mu.Unlock()
	state := "ok"
	if res.Bad > 0 {
		state = "bad"
	} else if res.Slow > 0 {
		state = "slow"
	}
	j.set("state", state)
	j.set("badCells", res.Bad)
	j.set("slowCells", res.Slow)
	j.set("badBytes", res.BadBytes)
	j.set("avgMBs", res.AvgMBs)
	j.set("badRanges", len(s.Ranges))
	return nil
}

func jobFake(j *Job, d Disk) error {
	if err := prepareRaw(j, d.Number); err != nil {
		return err
	}
	j.setStep(1)
	size := realSize(d)
	j.say("info", "Writing a unique pattern into every megabyte, then reading it all back...")
	r, err := FakeCapacity(devPath(d.Number), size, j.progress, &j.Cancel)
	if err != nil {
		return err
	}
	rescan(j, d.Number)
	j.setStep(len(j.Steps))
	j.set("state", r.Verdict)
	j.set("claimed", r.Claimed)
	j.set("real", r.Real)
	j.set("mismatch", r.Mismatch)
	if r.Fake {
		j.say("fail", fmt.Sprintf("Claimed %s but only %s is real.", fmtBytes(r.Claimed), fmtBytes(r.Real)))
	} else {
		j.say("ok", "Every block read back correctly. The capacity is real.")
	}
	return nil
}

func jobBench(j *Job, d Disk) error {
	wd := ""
	if !isWindows {
		wd = os.TempDir()
	} else if len(d.Volumes) > 0 {
		wd = d.Volumes[0].Letter + `:\`
	} else {
		j.say("warn", "No drive letter - only read speed can be measured.")
	}
	r, err := Benchmark(devPath(d.Number), realSize(d), wd, func(k string, v float64) {
		j.set(k, round1(v))
		j.say("info", fmt.Sprintf("%s: %.1f", k, v))
	}, &j.Cancel)
	j.set("class", r.Class)
	j.set("v", r.V)
	j.set("a", r.A)
	j.set("state", "ok")
	if wd == "" {
		j.set("noWrite", true)
	}
	return err
}

func jobShred(j *Job, paths []string, passes int) error {
	files, bytes := ShredPaths(paths, passes, func(p string, ok bool, err error) {
		if ok {
			j.say("ok", "Shredded "+p)
		} else {
			j.say("fail", fmt.Sprintf("%s: %v", p, err))
		}
		j.mu.Lock()
		j.Prog.Found++
		j.mu.Unlock()
	}, &j.Cancel)
	j.set("state", "ok")
	j.set("files", files)
	j.set("bytes", bytes)
	return nil
}

func jobVault(j *Job, mode, path, out, pw string) error {
	prog := func(done, total int64) {
		j.progress(Progress{Done: done, Total: max(total, 1)})
	}
	if mode == "enc" {
		if err := VaultEncrypt(path, out, pw, prog, &j.Cancel); err != nil {
			os.Remove(out)
			return err
		}
		j.set("file", out)
		j.say("ok", "Vault created: "+out)
	} else {
		n, err := VaultDecrypt(path, out, pw, prog, &j.Cancel)
		if err != nil {
			j.set("state", "wrongpw")
			return err
		}
		j.Outdir = out
		j.set("files", n)
		j.say("ok", fmt.Sprintf("Extracted %d files to %s", n, out))
	}
	j.set("state", "ok")
	return nil
}

// generic PowerShell-backed job
func jobPS(j *Job, script string, env []string, args ...string) error {
	if !isWindows {
		for i := range j.Steps {
			time.Sleep(450 * time.Millisecond)
			j.setStep(i)
		}
		j.setStep(len(j.Steps))
		j.say("ok", "(demo) done")
		j.set("state", "ok")
		return nil
	}
	err := psStream(j, script, env, args...)
	j.setStep(len(j.Steps))
	if _, ok := j.Result["state"]; !ok {
		j.set("state", "failed")
	}
	return err
}
