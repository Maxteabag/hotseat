package main

import (
	tea "charm.land/bubbletea/v2"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/Maxteabag/hotseat/internal/tui"
	"os"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() { os.Exit(run()) }
func run() int {
	showVersion := flag.Bool("version", false, "Print the version and exit")
	demoFile := flag.String("demo-file", "", "JSON fixture for offline demos; account actions are disabled")
	demo := flag.Bool("demo", false, "Show synthetic accounts; no backend calls or actions")
	render := flag.Bool("render", false, "Render one demo frame and exit")
	width := flag.Int("width", 110, "Width for --render")
	height := flag.Int("height", 32, "Height for --render")
	python := flag.String("python", "python3", "Python interpreter with Hotseat installed")
	root := flag.String("backend-root", tui.FindRoot(), "Hotseat checkout (otherwise use installed Python module)")
	snapshot := flag.Bool("snapshot", false, "Read a live snapshot as JSON without entering the TUI")
	flag.Parse()
	if *showVersion {
		fmt.Println("hotseat-tui " + version)
		return 0
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backend := &tui.PythonBackend{Python: *python, Root: *root}
	defer backend.Close()
	if *snapshot {
		s, e := backend.Snapshot(ctx, true)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(s)
		return 0
	}
	m := tui.New(ctx, backend)
	if *demo || *render {
		m = tui.Demo(ctx)
	}
	if *demoFile != "" {
		raw, err := os.ReadFile(*demoFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		var fixture struct {
			tui.Snapshot
			Work tui.WorkSnapshot `json:"work"`
		}
		if err = json.Unmarshal(raw, &fixture); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		m = tui.DemoWithData(ctx, fixture.Snapshot, fixture.Work)
	}
	if *render {
		_, _ = m.Update(tea.WindowSizeMsg{Width: *width, Height: *height})
		fmt.Print(m.View().Content)
		return 0
	}
	if _, err := tea.NewProgram(m, tea.WithContext(ctx)).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		cancel()
		return 1
	}

	return 0
}
