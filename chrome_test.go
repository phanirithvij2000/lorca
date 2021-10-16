package lorca

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TODO: made headless controllable via env "NO_HEADLESS"

var args = []string{"--remote-debugging-port=0", "--headless"}

func init() {
	tmpdir, _ := os.MkdirTemp("", "profile")
	// FIXED: on windows it hangs in --headless mode
	// chrome.exe --enable-logging=stderr --headless gave the warning
	// [1016/185032.379:WARNING:headless_browser_main_parts.cc(106)] Cannot create Pref Service with no user data dir
	// SO --user-data-dir is required for tests to run on Windows in headless mode
	// The bug was we were using /tmp previously
	// which doesn't exist on windows
	args = append(args, "--user-data-dir="+tmpdir)
}

func TestChromeEval(t *testing.T) {
	c, err := NewChromeWithArgs(LocateChrome(), args...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Kill()

	for _, test := range []struct {
		Expr   string
		Result string
		Error  string
	}{
		{Expr: ``, Result: ``},
		{Expr: `42`, Result: `42`},
		{Expr: `2+3`, Result: `5`},
		{Expr: `(() => ({x: 5, y: 7}))()`, Result: `{"x":5,"y":7}`},
		{Expr: `(() => ([1,'foo',false]))()`, Result: `[1,"foo",false]`},
		{Expr: `((a, b) => a*b)(3, 7)`, Result: `21`},
		{Expr: `Promise.resolve(42)`, Result: `42`},
		{Expr: `Promise.reject('foo')`, Error: `"foo"`},
		{Expr: `throw "bar"`, Error: `"bar"`},
		{Expr: `2+`, Error: `SyntaxError: Unexpected end of input`},
	} {
		result, err := c.Eval(test.Expr)
		if err != nil {
			if err.Error() != test.Error {
				t.Fatal(test.Expr, err, test.Error)
			}
		} else if string(result) != test.Result {
			t.Fatal(test.Expr, string(result), test.Result)
		}
	}
}

func TestChromeLoad(t *testing.T) {
	c, err := NewChromeWithArgs(LocateChrome(), args...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Kill()
	if err := c.Load("data:text/html,<html><body>Hello</body></html>"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		url, err := c.Eval(`window.location.href`)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(string(url), `"data:text/html,`) {
			break
		}
	}
	if res, err := c.Eval(`document.body ? document.body.innerText :
			new Promise(res => window.onload = () => res(document.body.innerText))`); err != nil {
		t.Fatal(err)
	} else if string(res) != `"Hello"` {
		t.Fatal(res)
	}
}

func TestChromeBind(t *testing.T) {
	c, err := NewChromeWithArgs(LocateChrome(), args...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Kill()

	if err := c.Bind("add", func(args []json.RawMessage) (interface{}, error) {
		a, b := 0, 0
		if len(args) != 2 {
			return nil, errors.New("2 arguments expected")
		}
		if err := json.Unmarshal(args[0], &a); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(args[1], &b); err != nil {
			return nil, err
		}
		return a + b, nil
	}); err != nil {
		t.Fatal(err)
	}

	if res, err := c.Eval(`window.add(2, 3)`); err != nil {
		t.Fatal(err)
	} else if string(res) != `5` {
		t.Fatal(string(res))
	}

	if res, err := c.Eval(`window.add("foo", "bar")`); err == nil {
		t.Fatal(string(res), err)
	}
	if res, err := c.Eval(`window.add(1, 2, 3)`); err == nil {
		t.Fatal(res, err)
	}
}

func TestChromeAsync(t *testing.T) {
	c, err := NewChromeWithArgs(LocateChrome(), args...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Kill()

	if err := c.Bind("len", func(args []json.RawMessage) (interface{}, error) {
		return len(args[0]), nil
	}); err != nil {
		t.Fatal(err)
	}

	wg := &sync.WaitGroup{}
	n := 10
	failed := int32(0)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			v, err := c.Eval("len('hello')")
			if string(v) != `7` {
				atomic.StoreInt32(&failed, 1)
			} else if err != nil {
				atomic.StoreInt32(&failed, 2)
			}
		}(i)
	}
	wg.Wait()

	if status := atomic.LoadInt32(&failed); status != 0 {
		t.Fatal()
	}
}
