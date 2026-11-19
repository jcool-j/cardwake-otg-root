package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func findBrowser() string {
	var c []string
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LocalAppData"} {
		if b := os.Getenv(env); b != "" {
			c = append(c,
				filepath.Join(b, "Microsoft", "Edge", "Application", "msedge.exe"),
				filepath.Join(b, "Google", "Chrome", "Application", "chrome.exe"),
				filepath.Join(b, "BraveSoftware", "Brave-Browser", "Application", "brave.exe"))
		}
	}
	for _, p := range c {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	for _, n := range []string{"msedge", "chrome", "chromium"} {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func main() {
	port := flag.Int("port", 0, "port (0 = random)")
	noWindow := flag.Bool("no-window", false, "do not open a window")
	noAdmin := flag.Bool("no-admin", false, "do not ask for administrator rights")
	demoImage := flag.String("demo-image", "", "demo mode (non-Windows): disk image used as the card")
	demoPick := flag.String("demo-any", "", "demo mode: path returned by file pickers")
	flag.Parse()

	if isWindows && !*noAdmin && !isAdmin() {
		elevate("")
		return
	}
	if !isWindows {
		if *demoImage != "" {
			tmp := filepath.Join(os.TempDir(), "cardwake_demo_dev.img")
			if err := copyFile(*demoImage, tmp); err != nil {
				fmt.Fprintln(os.Stderr, "demo image:", err)
				os.Exit(1)
			}
			demoDev = tmp
		}
		demoAny = *demoPick
	}
	token = newToken()
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/?t=%s", ln.Addr().(*net.TCPAddr).Port, token)
	srv := &http.Server{Handler: buildMux()}
	profile, _ := os.MkdirTemp("", "cardwake_ui_")
	defer os.RemoveAll(profile)

	if *noWindow {
		fmt.Println(url)
	} else {
		go watchdog(srv)
		if b := findBrowser(); b != "" {
			c := exec.Command(b, "--app="+url, "--user-data-dir="+profile, "--window-size=1220,840", "--no-first-run",
				"--no-default-browser-check", "--disable-extensions", "--disable-features=Translate,msEdgeTranslate")
			c.Start()
		} else {
			openPath(url)
		}
	}
	srv.Serve(ln)
}

func watchdog(srv *http.Server) {
	start := time.Now()
	for range time.Tick(time.Second) {
		now := time.Now().Unix()
		jobMu.Lock()
		busy := curJob != nil && curJob.Status == "running"
		jobMu.Unlock()
		if b := byeAt.Load(); b != 0 && now > b {
			break
		}
		if !everPinged.Load() && time.Since(start) > 2*time.Minute {
			break
		}
		limit := int64(600)
		if busy {
			limit = 900
		}
		if everPinged.Load() && now-lastPing.Load() > limit {
			break
		}
	}
	srv.Close()
	os.Exit(0)
}
