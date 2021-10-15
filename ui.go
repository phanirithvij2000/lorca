package lorca

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"reflect"
)

// UI interface allows talking to the HTML5 UI from Go.
type UIInt interface {
	Load(url string) error
	Bounds() (Bounds, error)
	SetBounds(Bounds) error
	Bind(name string, f interface{}) error
	Eval(js string) Value
	Done() <-chan struct{}
	Close() error
	Dir() string
}

type UI struct {
	*Chrome
	done   chan struct{}
	tmpDir string
	dir    string
}

var defaultChromeArgs = []string{
	"--disable-background-networking",
	"--disable-background-timer-throttling",
	"--disable-backgrounding-occluded-windows",
	"--disable-breakpad",
	"--disable-client-side-phishing-detection",
	"--disable-default-apps",
	"--disable-dev-shm-usage",
	"--disable-infobars",
	"--disable-extensions",
	"--disable-features=site-per-process",
	"--disable-hang-monitor",
	"--disable-ipc-flooding-protection",
	"--disable-popup-blocking",
	"--disable-prompt-on-repost",
	"--disable-renderer-backgrounding",
	"--disable-sync",
	"--disable-translate",
	"--disable-windows10-custom-titlebar",
	"--metrics-recording-only",
	"--no-first-run",
	"--no-default-browser-check",
	"--safebrowsing-disable-auto-update",
	"--disable-automation",
	"--password-store=basic",
	"--use-mock-keychain",
}

// New returns a new HTML5 UI for the given URL, user profile directory, window
// size and other options passed to the browser engine. If URL is an empty
// string - a blank page is displayed. If user profile directory is an empty
// string - a temporary directory is created and it will be removed on
// ui.Close(). You might want to use "--headless" custom CLI argument to test
// your UI code.
func New(chromeExe, url, userDataDir string, width, height int, customArgs ...string) (*UI, error) {
	if url == "" {
		url = "data:text/html,<html></html>"
	}
	tmpDir := ""
	if userDataDir == "" {
		name, err := ioutil.TempDir("", "lorca")
		if err != nil {
			return nil, err
		}
		userDataDir, tmpDir = name, name
	}
	args := append(defaultChromeArgs, fmt.Sprintf("--app=%s", url))
	args = append(args, fmt.Sprintf("--user-data-dir=%s", userDataDir))
	args = append(args, fmt.Sprintf("--window-size=%d,%d", width, height))
	args = append(args, customArgs...)
	args = append(args, "--remote-debugging-port=0")

	chrome, err := NewChromeWithArgs(chromeExe, args...)
	done := make(chan struct{})
	if err != nil {
		return nil, err
	}

	go func() {
		chrome.Cmd.Wait()
		close(done)
	}()
	return &UI{Chrome: chrome, done: done, tmpDir: tmpDir}, nil
}

// NewWithPreCallback same as New but with callbacks
// preCallback which runs before browser is launched
func NewWithPreCallback(url, dir string, width, height int, preCallback func(UIInt), customArgs ...string) (UIInt, error) {
	if url == "" {
		url = "data:text/html,<html></html>"
	}
	tmpDir := ""
	if dir == "" {
		name, err := ioutil.TempDir("", "lorca")
		if err != nil {
			return nil, err
		}
		dir, tmpDir = name, name
	}
	args := append(defaultChromeArgs, fmt.Sprintf("--app=%s", url))
	args = append(args, fmt.Sprintf("--user-data-dir=%s", dir))
	args = append(args, fmt.Sprintf("--window-size=%d,%d", width, height))
	args = append(args, customArgs...)
	args = append(args, "--remote-debugging-port=0")

	retUi := new(UI)
	retUi.tmpDir = tmpDir
	retUi.dir = dir

	done := make(chan struct{})
	retUi.done = done

	preCallback(retUi)

	chrome, err := NewChromeWithArgs(LocateChrome(), args...)
	if err != nil {
		return nil, err
	}

	go func() {
		chrome.Cmd.Wait()
		close(done)
	}()
	retUi.Chrome = chrome

	return retUi, nil
}

func (u *UI) Dir() string {
	if u.tmpDir != "" {
		return u.tmpDir
	}
	return u.dir
}

func (u *UI) Done() <-chan struct{} {
	return u.done
}

func (u *UI) Close() error {
	// ignore err, as the chrome process might be already dead, when user close the window.
	u.Chrome.Kill()
	<-u.done
	if u.tmpDir != "" {
		if err := os.RemoveAll(u.tmpDir); err != nil {
			return err
		}
	}
	return nil
}

func (u *UI) Bind(name string, f interface{}) error {
	v := reflect.ValueOf(f)
	// f must be a function
	if v.Kind() != reflect.Func {
		return errors.New("only functions can be bound")
	}
	// f must return either value and error or just error
	if n := v.Type().NumOut(); n > 2 {
		return errors.New("function may only return a value or a value+error")
	}

	return u.Chrome.Bind(name, func(raw []json.RawMessage) (interface{}, error) {
		if len(raw) != v.Type().NumIn() {
			return nil, errors.New("function arguments mismatch")
		}
		args := []reflect.Value{}
		for i := range raw {
			arg := reflect.New(v.Type().In(i))
			if err := json.Unmarshal(raw[i], arg.Interface()); err != nil {
				return nil, err
			}
			args = append(args, arg.Elem())
		}
		errorType := reflect.TypeOf((*error)(nil)).Elem()
		res := v.Call(args)
		switch len(res) {
		case 0:
			// No results from the function, just return nil
			return nil, nil
		case 1:
			// One result may be a value, or an error
			if res[0].Type().Implements(errorType) {
				if res[0].Interface() != nil {
					return nil, res[0].Interface().(error)
				}
				return nil, nil
			}
			return res[0].Interface(), nil
		case 2:
			// Two results: first one is value, second is error
			if !res[1].Type().Implements(errorType) {
				return nil, errors.New("second return value must be an error")
			}
			if res[1].Interface() == nil {
				return res[0].Interface(), nil
			}
			return res[0].Interface(), res[1].Interface().(error)
		default:
			return nil, errors.New("unexpected number of return values")
		}
	})
}

func (u *UI) Eval(js string) Value {
	v, err := u.Chrome.Eval(js)
	return value{err: err, raw: v}
}
