package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"

	"github.com/Maxteabag/hotseat/internal/collect"
	"github.com/Maxteabag/hotseat/internal/tui"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() { os.Exit(run()) }
func run() int {
	if len(os.Args) > 1 && os.Args[1] == "bridge" {
		return bridge(os.Args[2:])
	}
	showVersion := flag.Bool("version", false, "Print the version and exit")
	demoFile := flag.String("demo-file", "", "JSON fixture for offline demos; account actions are disabled")
	demo := flag.Bool("demo", false, "Show synthetic accounts; no backend calls or actions")
	render := flag.Bool("render", false, "Render one demo frame and exit")
	width := flag.Int("width", 110, "Width for --render")
	height := flag.Int("height", 32, "Height for --render")
	snapshot := flag.Bool("snapshot", false, "Read a live snapshot as JSON without entering the TUI")
	flag.Parse()
	if *showVersion {
		fmt.Println("hotseat-tui " + version)
		return 0
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backend := collect.New()
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

// bridge is the hidden verification mode: it prints the one-line JSON that
// `python -m hotseat.tui_bridge <operation>` printed, so scripts/compare_bridge.py
// can diff the two implementations on a real machine. Read-only operations only.
func bridge(args []string) int {
	flags := flag.NewFlagSet("hotseat-tui bridge", flag.ContinueOnError)
	refresh := flags.Bool("refresh", false, "Read quota live")
	provider := flags.String("provider", "", "Provider of the work item (inspect-work)")
	id := flags.String("id", "", "Identifier of the work item (inspect-work)")
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: hotseat-tui bridge snapshot [--refresh] | work | inspect-work --provider P --id ID")
		return 2
	}
	operation := args[0]
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	ctx := context.Background()
	b := collect.New()
	var output any
	var err error
	switch operation {
	case "snapshot":
		output, err = b.Rows(ctx, *refresh)
	case "work":
		output = b.Listing(ctx)
	case "inspect-work":
		if *provider == "" || *id == "" {
			err = errors.New("inspect-work requires --provider and --id")
		} else {
			output, err = b.Detail(ctx, *provider, *id)
		}
	default:
		err = fmt.Errorf("unsupported bridge operation %q", operation)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	if err != nil {
		_ = encoder.Encode(map[string]string{"error": err.Error()})
		return 1
	}
	if err := encoder.Encode(output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
