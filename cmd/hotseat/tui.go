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

// tuiOptions are the flags of `hotseat tui`. Beyond --demo, which the Python
// documented, the rest exist for offline rendering and tests.
type tuiOptions struct {
	Demo     bool
	DemoFile string
	Render   bool
	Width    int
	Height   int
	Snapshot bool
}

// cmdTUI runs the Bubble Tea account desk in-process.
func (a *app) cmdTUI(cmd *command, ctx context.Context, args []string) (int, error) {
	fs := a.flags(cmd)
	var options tuiOptions
	fs.BoolVar(&options.Demo, "demo", false, "Synthetic data; no account actions")
	fs.StringVar(&options.DemoFile, "demo-file", "", "JSON fixture for offline demos; account actions are disabled")
	fs.BoolVar(&options.Render, "render", false, "Render one demo frame and exit")
	fs.IntVar(&options.Width, "width", 110, "Width for --render")
	fs.IntVar(&options.Height, "height", 32, "Height for --render")
	fs.BoolVar(&options.Snapshot, "snapshot", false, "Read a live snapshot as JSON without entering the TUI")
	if _, err := a.parse(cmd, fs, args, 0, 0); err != nil {
		return 2, err
	}
	return a.runTUI(ctx, options), nil
}

func (a *app) realTUI(ctx context.Context, options tuiOptions) int {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	backend := collect.New()
	if options.Snapshot {
		s, err := backend.Snapshot(ctx, true)
		if err != nil {
			a.errorln(err.Error())
			return 1
		}
		_ = json.NewEncoder(a.stdout).Encode(s)
		return 0
	}
	m := tui.New(ctx, backend)
	if options.Demo || options.Render {
		m = tui.Demo(ctx)
	}
	if options.DemoFile != "" {
		raw, err := os.ReadFile(options.DemoFile)
		if err != nil {
			a.errorln(err.Error())
			return 1
		}
		var fixture struct {
			tui.Snapshot
			Work tui.WorkSnapshot `json:"work"`
		}
		if err = json.Unmarshal(raw, &fixture); err != nil {
			a.errorln(err.Error())
			return 1
		}
		m = tui.DemoWithData(ctx, fixture.Snapshot, fixture.Work)
	}
	if options.Render {
		_, _ = m.Update(tea.WindowSizeMsg{Width: options.Width, Height: options.Height})
		fmt.Fprint(a.stdout, m.View().Content)
		return 0
	}
	if _, err := tea.NewProgram(m, tea.WithContext(ctx)).Run(); err != nil {
		a.errorln(err.Error())
		return 1
	}
	return 0
}

// realBridge is the hidden verification mode: it prints the one-line JSON that
// `python -m hotseat.tui_bridge <operation>` printed, so scripts/compare_bridge.py
// can diff the two implementations on a real machine. Read-only operations only.
func (a *app) realBridge(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("hotseat bridge", flag.ContinueOnError)
	flags.SetOutput(a.stderr)
	refresh := flags.Bool("refresh", false, "Read quota live")
	provider := flags.String("provider", "", "Provider of the work item (inspect-work)")
	id := flags.String("id", "", "Identifier of the work item (inspect-work)")
	if len(args) == 0 {
		a.errorln("usage: hotseat bridge snapshot [--refresh] | work | inspect-work --provider P --id ID")
		return 2
	}
	operation := args[0]
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
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
	encoder := json.NewEncoder(a.stdout)
	encoder.SetEscapeHTML(false)
	if err != nil {
		_ = encoder.Encode(map[string]string{"error": err.Error()})
		return 1
	}
	if err := encoder.Encode(output); err != nil {
		a.errorln(err.Error())
		return 1
	}
	return 0
}
