# Regenerate the README screenshots with VHS (https://github.com/charmbracelet/vhs).
#
#   go build -o bin/hotseat ./cmd/hotseat
#   vhs docs/screenshots.tape
#
# Everything shown comes from docs/demo-fixture.json; no account is read.

Set Shell bash
Set FontFamily "JetBrains Mono"
Set FontSize 15
Set Width 1180
Set Height 560
Set Padding 16
Set Theme { "background": "#171b24", "foreground": "#d6dce8", "cursor": "#171b24" }

Hide
Type "bin/hotseat tui --demo-file docs/demo-fixture.json"
Enter
Sleep 2s
Show
Sleep 1s

Screenshot docs/tui.png
Sleep 500ms
Type "2"
Sleep 1500ms
Screenshot docs/tui-claude.png
Sleep 500ms
Type "w"
Sleep 800ms
Type "1"
Sleep 1500ms
Screenshot docs/tui-work.png
Sleep 500ms
Type "q"
Sleep 300ms
