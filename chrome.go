package lorca

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os/exec"
	"regexp"
	"sync"
	"sync/atomic"

	"golang.org/x/net/websocket"
)

type h = map[string]interface{}

// Result is a struct for the resulting value of the JS expression or an error.
type result struct {
	Value json.RawMessage
	Err   error
}

type bindingFunc func(args []json.RawMessage) (interface{}, error)

// Msg is a struct for incoming messages (results and async events)
type msg struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// Chrome represents a chrome process
type Chrome struct {
	sync.Mutex
	Cmd      *exec.Cmd
	ws       *websocket.Conn
	id       int32
	target   string
	session  string
	window   int
	pending  map[int]chan result
	bindings map[string]bindingFunc
}

// NewChromeWithArgs starts chrome process with arguments
func NewChromeWithArgs(chromeBinary string, args ...string) (*Chrome, error) {
	// The first two IDs are used internally during the initialization
	c := &Chrome{
		id:       2,
		pending:  map[int]chan result{},
		bindings: map[string]bindingFunc{},
	}

	c.Cmd = exec.Command(chromeBinary, args...)
	pipe, err := c.Cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := c.Cmd.Start(); err != nil {
		return nil, err
	}

	// Wait for websocket address to be printed to stderr
	re := regexp.MustCompile(`^DevTools listening on (ws://.*?)\r?\n$`)
	m, err := readUntilMatch(pipe, re)
	if err != nil {
		c.Kill()
		return nil, err
	}
	wsURL := m[1]

	// Open a websocket
	c.ws, err = websocket.Dial(wsURL, "", "http://127.0.0.1")
	if err != nil {
		c.Kill()
		return nil, err
	}

	// Find target and initialize session
	c.target, err = c.findTarget()
	if err != nil {
		c.Kill()
		return nil, err
	}

	c.session, err = c.startSession(c.target)
	if err != nil {
		c.Kill()
		return nil, err
	}
	go c.readLoop()
	for method, args := range map[string]h{
		"Page.enable":          nil,
		"Target.setAutoAttach": {"autoAttach": true, "waitForDebuggerOnStart": false},
		"Network.enable":       nil,
		"Runtime.enable":       nil,
		"Security.enable":      nil,
		"Performance.enable":   nil,
		"Log.enable":           nil,
	} {
		if _, err := c.Send(method, args); err != nil {
			c.Kill()
			c.Cmd.Wait()
			return nil, err
		}
	}

	if !contains(args, "--headless") {
		win, err := c.getWindowForTarget(c.target)
		if err != nil {
			c.Kill()
			return nil, err
		}
		c.window = win.WindowID
	}

	return c, nil
}

func (c *Chrome) findTarget() (string, error) {
	err := websocket.JSON.Send(c.ws, h{
		"id": 0, "method": "Target.setDiscoverTargets", "params": h{"discover": true},
	})
	if err != nil {
		return "", err
	}
	for {
		m := msg{}
		if err = websocket.JSON.Receive(c.ws, &m); err != nil {
			return "", err
		} else if m.Method == "Target.targetCreated" {
			target := struct {
				TargetInfo struct {
					Type string `json:"type"`
					ID   string `json:"targetId"`
				} `json:"targetInfo"`
			}{}
			if err := json.Unmarshal(m.Params, &target); err != nil {
				return "", err
			} else if target.TargetInfo.Type == "page" {
				return target.TargetInfo.ID, nil
			}
		}
	}
}

func (c *Chrome) startSession(target string) (string, error) {
	err := websocket.JSON.Send(c.ws, h{
		"id": 1, "method": "Target.attachToTarget", "params": h{"targetId": target},
	})
	if err != nil {
		return "", err
	}
	for {
		m := msg{}
		if err = websocket.JSON.Receive(c.ws, &m); err != nil {
			return "", err
		} else if m.ID == 1 {
			if m.Error != nil {
				return "", errors.New("Target error: " + string(m.Error))
			}
			session := struct {
				ID string `json:"sessionId"`
			}{}
			if err := json.Unmarshal(m.Result, &session); err != nil {
				return "", err
			}
			return session.ID, nil
		}
	}
}

// WindowState defines the state of the Chrome window, possible values are
// "normal", "maximized", "minimized" and "fullscreen".
type WindowState string

const (
	// WindowStateNormal defines a normal state of the browser window
	WindowStateNormal WindowState = "normal"
	// WindowStateMaximized defines a maximized state of the browser window
	WindowStateMaximized WindowState = "maximized"
	// WindowStateMinimized defines a minimized state of the browser window
	WindowStateMinimized WindowState = "minimized"
	// WindowStateFullscreen defines a fullscreen state of the browser window
	WindowStateFullscreen WindowState = "fullscreen"
)

// Bounds defines settable window properties.
type Bounds struct {
	Left        int         `json:"left"`
	Top         int         `json:"top"`
	Width       int         `json:"width"`
	Height      int         `json:"height"`
	WindowState WindowState `json:"windowState"`
}

type windowTargetMessage struct {
	WindowID int    `json:"windowId"`
	Bounds   Bounds `json:"bounds"`
}

func (c *Chrome) getWindowForTarget(target string) (windowTargetMessage, error) {
	var m windowTargetMessage
	msg, err := c.Send("Browser.getWindowForTarget", h{"targetId": target})
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(msg, &m)
	return m, err
}

type targetMessageTemplate struct {
	ID     int    `json:"id"`
	Method string `json:"method"`
	Params struct {
		Name    string `json:"name"`
		Payload string `json:"payload"`
		ID      int    `json:"executionContextId"`
		Args    []struct {
			Type  string      `json:"type"`
			Value interface{} `json:"value"`
		} `json:"args"`
	} `json:"params"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
	Result json.RawMessage `json:"result"`
}

type targetMessage struct {
	targetMessageTemplate
	Result struct {
		Result struct {
			Type        string          `json:"type"`
			Subtype     string          `json:"subtype"`
			Description string          `json:"description"`
			Value       json.RawMessage `json:"value"`
			ObjectID    string          `json:"objectId"`
		} `json:"result"`
		Exception struct {
			Exception struct {
				Value json.RawMessage `json:"value"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	} `json:"result"`
}

func (c *Chrome) readLoop() {
	for {
		m := msg{}
		if err := websocket.JSON.Receive(c.ws, &m); err != nil {
			return
		}

		if m.Method == "Target.receivedMessageFromTarget" {
			params := struct {
				SessionID string `json:"sessionId"`
				Message   string `json:"message"`
			}{}
			json.Unmarshal(m.Params, &params)
			if params.SessionID != c.session {
				continue
			}
			res := targetMessage{}
			json.Unmarshal([]byte(params.Message), &res)

			if res.ID == 0 && res.Method == "Runtime.consoleAPICalled" || res.Method == "Runtime.exceptionThrown" {
				log.Println(params.Message)
			} else if res.ID == 0 && res.Method == "Runtime.bindingCalled" {
				payload := struct {
					Name string            `json:"name"`
					Seq  int               `json:"seq"`
					Args []json.RawMessage `json:"args"`
				}{}
				json.Unmarshal([]byte(res.Params.Payload), &payload)

				c.Lock()
				binding, ok := c.bindings[res.Params.Name]
				c.Unlock()
				if ok {
					jsString := func(v interface{}) string { b, _ := json.Marshal(v); return string(b) }
					go func() {
						result, error := "", `""`
						if r, err := binding(payload.Args); err != nil {
							error = jsString(err.Error())
						} else if b, err := json.Marshal(r); err != nil {
							error = jsString(err.Error())
						} else {
							result = string(b)
						}
						expr := fmt.Sprintf(`
							if (%[4]s) {
								window['%[1]s']['errors'].get(%[2]d)(%[4]s);
							} else {
								window['%[1]s']['callbacks'].get(%[2]d)(%[3]s);
							}
							window['%[1]s']['callbacks'].delete(%[2]d);
							window['%[1]s']['errors'].delete(%[2]d);
							`, payload.Name, payload.Seq, result, error)
						c.Send("Runtime.evaluate", h{"expression": expr, "contextId": res.Params.ID})
					}()
				}
				continue
			}

			c.Lock()
			resc, ok := c.pending[res.ID]
			delete(c.pending, res.ID)
			c.Unlock()

			if !ok {
				continue
			}

			if res.Error.Message != "" {
				resc <- result{Err: errors.New(res.Error.Message)}
			} else if res.Result.Exception.Exception.Value != nil {
				resc <- result{Err: errors.New(string(res.Result.Exception.Exception.Value))}
			} else if res.Result.Result.Type == "object" && res.Result.Result.Subtype == "error" {
				resc <- result{Err: errors.New(res.Result.Result.Description)}
			} else if res.Result.Result.Type != "" {
				resc <- result{Value: res.Result.Result.Value}
			} else {
				res := targetMessageTemplate{}
				json.Unmarshal([]byte(params.Message), &res)
				resc <- result{Value: res.Result}
			}
		} else if m.Method == "Target.targetDestroyed" {
			params := struct {
				TargetID string `json:"targetId"`
			}{}
			json.Unmarshal(m.Params, &params)
			if params.TargetID == c.target {
				c.Kill()
				return
			}
		}
	}
}

// Send sends a method with a parameters to the browser, waits for response
// and returns response as json
func (c *Chrome) Send(method string, params h) (json.RawMessage, error) {
	id := atomic.AddInt32(&c.id, 1)
	b, err := json.Marshal(h{"id": int(id), "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	resc := make(chan result)
	c.Lock()
	c.pending[int(id)] = resc
	c.Unlock()

	if err := websocket.JSON.Send(c.ws, h{
		"id":     int(id),
		"method": "Target.sendMessageToTarget",
		"params": h{"message": string(b), "sessionId": c.session},
	}); err != nil {
		return nil, err
	}
	res := <-resc
	return res.Value, res.Err
}

// Load navigates to a given URL
func (c *Chrome) Load(url string) error {
	_, err := c.Send("Page.navigate", h{"url": url})
	return err
}

// Eval evaluates JavaScript expression in the browser and returns response
func (c *Chrome) Eval(expr string) (json.RawMessage, error) {
	return c.Send("Runtime.evaluate", h{"expression": expr, "awaitPromise": true, "returnByValue": true})
}

// AddScriptToEvaluateOnNewDocument adds JavaScript code to be evaluated
// when new document is evaluted
func (c *Chrome) AddScriptToEvaluateOnNewDocument(script string) error {
	_, err := c.Send("Page.addScriptToEvaluateOnNewDocument", h{"source": script})
	if err != nil {
		return err
	}
	_, err = c.Eval(script)
	return err
}

// Reload reloads current page
// https://pptr.dev/#?product=Puppeteer&show=api-pagereloadoptions
func (c *Chrome) Reload(disableCache bool) error {
	// TODO: should restore current cache setting
	if disableCache {
		c.Send("Network.setCacheDisabled", h{"cacheDisabled": true})
	}
	_, err := c.Send("Page.reload", h{"waitUntil": 0})
	if disableCache {
		c.Send("Network.setCacheDisabled", h{"cacheDisabled": false})
	}
	return err
}

type navigationHistoryEntry struct {
	ID int `json:"id"`
}

type navigationHistory struct {
	CurrentIndex int `json:"currentIndex"`
	Entries []*navigationHistoryEntry `json:"entries"`
}

func (c *Chrome) getNavigationHistory() (*navigationHistory, error) {
	result, err := c.Send("Page.getNavigationHistory", nil)
	if err != nil {
		return nil, err
	}
	fmt.Printf("Navigation history:\n%s\n", string(result))
	var h navigationHistory
	err = json.Unmarshal(result, &h)
	if err != nil {
		return nil, err
	}
	return &h, nil
}

func (c *Chrome) goDelta(delta int) error {
	history, err := c.getNavigationHistory()
	if err != nil {
		return err
	}
	n := history.CurrentIndex + delta
	if n < 0 || n >= len(history.Entries) {
		return fmt.Errorf("invalid delta %d, would navigate to %d which is outside of history length of %d", delta, n, len(history.Entries))
	}
	e := history.Entries[n]
	// TODO: maybe add a way to wait for navigation
	_, err = c.Send("Page.navigateToHistoryEntry", h{"entryId": e.ID})
	return err
}

// Back navigates to previous page in browser history
func (c *Chrome) Back() error {
	return c.goDelta(1)
}

// Forward navigates to next page in browser history
func (c *Chrome) Forward() error {
	return c.goDelta(1)
}

// Bind creates a browser-side binding with name to a function
func (c *Chrome) Bind(name string, f bindingFunc) error {
	c.Lock()
	c.bindings[name] = f
	c.Unlock()
	if _, err := c.Send("Runtime.addBinding", h{"name": name}); err != nil {
		return err
	}
	script := fmt.Sprintf(`(() => {
	const bindingName = '%s';
	const binding = window[bindingName];
	window[bindingName] = async (...args) => {
		const me = window[bindingName];
		let errors = me['errors'];
		let callbacks = me['callbacks'];
		if (!callbacks) {
			callbacks = new Map();
			me['callbacks'] = callbacks;
		}
		if (!errors) {
			errors = new Map();
			me['errors'] = errors;
		}
		const seq = (me['lastSeq'] || 0) + 1;
		me['lastSeq'] = seq;
		const promise = new Promise((resolve, reject) => {
			callbacks.set(seq, resolve);
			errors.set(seq, reject);
		});
		binding(JSON.stringify({name: bindingName, seq, args}));
		return promise;
	}})();
	`, name)
	_, err := c.Send("Page.addScriptToEvaluateOnNewDocument", h{"source": script})
	if err != nil {
		return err
	}
	_, err = c.Eval(script)
	return err
}

// SetBounds sets the size, position and state of a browser window
func (c *Chrome) SetBounds(b Bounds) error {
	if b.WindowState == "" {
		b.WindowState = WindowStateNormal
	}
	param := h{"windowId": c.window, "bounds": b}
	if b.WindowState != WindowStateNormal {
		param["bounds"] = h{"windowState": b.WindowState}
	}
	_, err := c.Send("Browser.setWindowBounds", param)
	return err
}

// Bounds returns the size, position and a state of a browser window
func (c *Chrome) Bounds() (Bounds, error) {
	result, err := c.Send("Browser.getWindowBounds", h{"windowId": c.window})
	if err != nil {
		return Bounds{}, err
	}
	bounds := struct {
		Bounds Bounds `json:"bounds"`
	}{}
	err = json.Unmarshal(result, &bounds)
	return bounds.Bounds, err
}

// PDF generates a PDF of a given size and returns the PDF content as []byte
func (c *Chrome) PDF(width, height int) ([]byte, error) {
	result, err := c.Send("Page.printToPDF", h{
		"paperWidth":  float32(width) / 96,
		"paperHeight": float32(height) / 96,
	})
	if err != nil {
		return nil, err
	}
	pdf := struct {
		Data []byte `json:"data"`
	}{}
	err = json.Unmarshal(result, &pdf)
	return pdf.Data, err
}

// PNG generates
func (c *Chrome) PNG(x, y, width, height int, bg uint32, scale float32) ([]byte, error) {
	if x == 0 && y == 0 && width == 0 && height == 0 {
		// By default either use SVG size if it's an SVG, or use A4 page size
		bounds, err := c.Eval(`document.rootElement ? [document.rootElement.x.baseVal.value, document.rootElement.y.baseVal.value, document.rootElement.width.baseVal.value, document.rootElement.height.baseVal.value] : [0,0,816,1056]`)
		if err != nil {
			return nil, err
		}
		rect := make([]int, 4)
		if err := json.Unmarshal(bounds, &rect); err != nil {
			return nil, err
		}
		x, y, width, height = rect[0], rect[1], rect[2], rect[3]
	}

	_, err := c.Send("Emulation.setDefaultBackgroundColorOverride", h{
		"color": h{
			"r": (bg >> 16) & 0xff,
			"g": (bg >> 8) & 0xff,
			"b": bg & 0xff,
			"a": (bg >> 24) & 0xff,
		},
	})
	if err != nil {
		return nil, err
	}
	result, err := c.Send("Page.captureScreenshot", h{
		"clip": h{
			"x": x, "y": y, "width": width, "height": height, "scale": scale,
		},
	})
	if err != nil {
		return nil, err
	}
	pdf := struct {
		Data []byte `json:"data"`
	}{}
	err = json.Unmarshal(result, &pdf)
	return pdf.Data, err
}

// Kill kills the chrome process
func (c *Chrome) Kill() error {
	if c.ws != nil {
		if err := c.ws.Close(); err != nil {
			return err
		}
	}
	// TODO: cancel all pending requests
	if state := c.Cmd.ProcessState; state == nil || !state.Exited() {
		return c.Cmd.Process.Kill()
	}
	return nil
}

// DisableContextMenu disables Chrome's default context menu on right mouse click
func (c *Chrome) DisableContextMenu() error {
	return c.AddScriptToEvaluateOnNewDocument(DisableContextMenuScript)
}

// DisableDefaultShortcuts disables default shortucts like Ctr-N to open a new tab
func (c *Chrome) DisableDefaultShortcuts() error {
	return c.AddScriptToEvaluateOnNewDocument(DisableShortcutsScript)
}

func readUntilMatch(r io.ReadCloser, re *regexp.Regexp) ([]string, error) {
	br := bufio.NewReader(r)
	for {
		if line, err := br.ReadString('\n'); err != nil {
			r.Close()
			return nil, err
		} else if m := re.FindStringSubmatch(line); m != nil {
			go io.Copy(ioutil.Discard, br)
			return m, nil
		}
	}
}

func contains(arr []string, x string) bool {
	for _, n := range arr {
		if x == n {
			return true
		}
	}
	return false
}
