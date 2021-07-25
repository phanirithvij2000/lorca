package main

import (
	"embed"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"

	"github.com/bitly/go-simplejson"
	"github.com/zserge/lorca"
)

//go:embed www
var fs embed.FS

// Go types that are bound to the UI must be thread-safe, because each binding
// is executed in its own goroutine. In this simple case we may use atomic
// operations, but for more complex cases one should use proper synchronization.
type counter struct {
	sync.Mutex
	count int
}

func (c *counter) Add(n int) {
	c.Lock()
	defer c.Unlock()
	c.count = c.count + n
}

func (c *counter) Value() int {
	c.Lock()
	defer c.Unlock()
	return c.count
}

// setBraveDefaultPrefs Sets Brave browser's default prefs
func setBraveDefaultPrefs(dir string) {
	if dir != "" {
		// mkdir if user-data-dir doesn't exist
		os.MkdirAll(dir, os.ModePerm)
	}
	localstate := filepath.Join(dir, "Local State")
	buf, _ := ioutil.ReadFile(localstate)
	// https://stackoverflow.com/a/16696204/8608146
	data, err := simplejson.NewJson(buf)
	if err != nil {
		data = simplejson.New()
	}
	data.SetPath([]string{"brave", "p3a", "notice_acknowledged"}, true)
	data.SetPath([]string{"brave", "p3a", "enabled"}, false)
	d, err := data.Encode()
	if err != nil {
		log.Println("Json encode failed:", err)
		return
	}
	err = ioutil.WriteFile(localstate, d, 0644)
	if err != nil {
		log.Println("Failed to write to Local State:", err)
	}
}

// removeCrashWarnings Removes any crash warnings
// and prevents from re-opening previous sessions
func removeCrashWarnings(dir string) {
	// If user-data-dir exists, remove crash warnings
	if _, err := os.Stat(dir); err == nil {
		// TODO use simplejson instead
		// Remove crash reload warnings
		prefs := filepath.Join(dir, "Default", "Preferences")
		var re = regexp.MustCompile(`"exit_type": *"Crashed"`)
		buf, err := ioutil.ReadFile(prefs)
		if err == nil {
			// https://superuser.com/questions/461035/disable-google-chrome-session-restore-functionality#comment2127608_886258
			s := re.ReplaceAll(buf, []byte(`"exit_type":"Normal"`))
			ioutil.WriteFile(prefs, s, 0644)
			log.Println("Removing any previous crash warnings")
		}
	}
}

func Normal(ui lorca.UI) {
	// log.Println("Normal window")
	ui.SetBounds(lorca.Bounds{WindowState: lorca.WindowStateNormal})
}

var fullScreenMinimized = false

func Minimize(ui lorca.UI) {
	// log.Println("Minimize window")
	b, _ := ui.Bounds()
	if b.WindowState == lorca.WindowStateFullscreen {
		Normal(ui)
		fullScreenMinimized = true
	}
	ui.SetBounds(lorca.Bounds{WindowState: lorca.WindowStateMinimized})
}

func Maximize(ui lorca.UI) {
	// log.Println("Maximize window")
	ui.SetBounds(lorca.Bounds{WindowState: lorca.WindowStateMaximized})
}

func ToggleMaximize(ui lorca.UI) {
	// log.Println("ToggleMax window")
	b, _ := ui.Bounds()
	if b.WindowState == lorca.WindowStateMaximized {
		Normal(ui)
	} else if b.WindowState == lorca.WindowStateNormal {
		Maximize(ui)
		// } else {
		// // Else should not be possible as
		// // we will hide fake window controls in FullScreen
		// // And in Minimized mode user can't click on them as ui is invisible
	}
}

func ToggleFullscreen(ui lorca.UI) {
	// log.Println("ToggleMax window")
	b, _ := ui.Bounds()
	if b.WindowState == lorca.WindowStateFullscreen {
		Normal(ui)
	} else if b.WindowState == lorca.WindowStateNormal {
		Fullscreen(ui)
	}
}

func Fullscreen(ui lorca.UI) {
	ui.SetBounds(lorca.Bounds{WindowState: lorca.WindowStateFullscreen})
}

var profileDir string

func init() {
	flag.StringVar(&profileDir, "profile", "", "user data directory for the browser")
}

func main() {

	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	args := []string{
		"--disable-infobars",
		// "--kiosk",
		"--disable-session-crashed-bubble",
		"--disable-experimental-fullscreen-exit-ui",
	}
	if runtime.GOOS == "linux" {
		args = append(args, "--class=Lorca")
	}

	userDataDir := ""
	if profileDir != "" {
		userDataDir, _ = filepath.Abs(profileDir)
	}

	// TODO add this as a feature in locra https://stackoverflow.com/a/54048350/8608146

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, http.FileServer(http.FS(fs)))

	log.Println("Listening on", fmt.Sprintf("http://%s", ln.Addr()))

	ui, err := lorca.NewWithPreCallback(
		fmt.Sprintf("http://%s/www", ln.Addr()),
		userDataDir,
		480, 320,
		func(ui lorca.UI) {
			removeCrashWarnings(userDataDir)
			setBraveDefaultPrefs(ui.Dir())
		},
		args...,
	)

	if err != nil {
		log.Fatal(err)
	}
	defer ui.Close()

	// A simple way to know when UI is ready (uses body.onload event in JS)
	ui.Bind("start", func() {
		log.Println("UI is ready")
	})

	ui.Bind("WindowFullscreen", func() { Fullscreen(ui) })
	ui.Bind("WindowMinimize", func() { Minimize(ui) })
	ui.Bind("WindowMaximize", func() { Maximize(ui) })
	ui.Bind("WindowToggleMax", func() { ToggleMaximize(ui) })
	ui.Bind("WindowToggleFullscreen", func() { ToggleFullscreen(ui) })
	ui.Bind("WindowIsFullscreen", func() bool {
		b, _ := ui.Bounds()
		return b.WindowState == lorca.WindowStateFullscreen
	})
	ui.Bind("WindowNormal", func() { Normal(ui) })
	ui.Bind("WindowClose", func() { ui.Close() })

	// set to fullscreen by default
	Fullscreen(ui)

	// Create and bind Go object to the UI
	c := &counter{}
	ui.Bind("counterAdd", c.Add)
	ui.Bind("counterValue", c.Value)

	// You may use console.log to debug your JS code, it will be printed via
	// log.Println(). Also exceptions are printed in a similar manner.
	ui.Eval(`
		console.log("Hello, world!");
		console.log('Multiple values:', [1, false, {"x":5}]);
	`)

	// Wait until the interrupt signal arrives or browser window is closed
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt)
	select {
	case <-sigc:
	case <-ui.Done():
	}

	log.Println("exiting...")
}
