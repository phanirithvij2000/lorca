package main

import (
	"log"
	"net/url"

	"github.com/phanirithvij2000/lorca"
)

func main() {
	// Create UI with basic HTML passed via data URI
	ui, err := lorca.New(lorca.LocateChrome(), "data:text/html,"+url.PathEscape(`
	<html>
		<head><title>Hello</title></head>
		<body><h1>Hello, world!</h1></body>
	</html>
	`), "", 480, 320, "--devtools-flags")
	if err != nil {
		log.Fatal(err)
	}
	ui.Chrome.DisableDefaultShortcuts()
	ui.Chrome.DisableContextMenu()

	defer ui.Close()
	// Wait until UI window is closed
	<-ui.Done()
}
