# Records site/public/demo.gif. Run `make demo` (it builds ./bin/tobira and seeds
# /tmp/tobira-demo.db first). Keys come from docs/keymaps (default keymap).
Output site/public/demo.gif

Require ./bin/tobira

Set Shell "bash"
Set FontSize 22
Set Width 1100
Set Height 640
Set Framerate 15
Set Padding 12
Set Theme "Catppuccin Mocha"
Set TypingSpeed 60ms

Env TOBIRA_CONFIG "/tmp/tobira-demo/config.yaml"
Env PS1 "$ "

Type "./bin/tobira connect shop"
Enter
Sleep 2s

# Browse: expand the schema, open the customers collection.
Down
Enter
Sleep 1s
Down
Sleep 400ms
Enter
Sleep 1.5s
Down 2
Sleep 600ms

# Filter rows.
Type "f"
Sleep 400ms
Type "city = 'Tokyo'"
Sleep 800ms
Enter
Sleep 2s

# Run a query in the editor.
Ctrl+E
Sleep 500ms
Type "select city, count(*) as n from customers group by city order by n desc"
Sleep 800ms
Alt+Enter
Sleep 3s

# Back to the filtered grid: edit a cell inline and apply.
Ctrl+E
Sleep 500ms
Type "["
Sleep 800ms
Right
Sleep 300ms
Type "e"
Sleep 600ms
Type " VIP"
Sleep 600ms
Enter
Sleep 1.2s
Type ":"
Sleep 300ms
Type "apply"
Sleep 600ms
Enter
Sleep 2.5s

Type "q"
Sleep 800ms
