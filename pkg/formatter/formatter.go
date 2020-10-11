package formatter

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"io/ioutil"

	"git.sr.ht/~danieljamespost/nyne/pkg/event"
	"git.sr.ht/~danieljamespost/nyne/util/config"
)

// Formatter listens for Acme events and applies formatting rules to the active buffer
type Formatter interface {
	Run()
	ExecCmds(event event.Event, commands []config.Command, ext string) error
	WriteMenu(w *event.Win) error
	SetupFormatting(*event.Win, Fmt) error
	Refmt(event.Event, string, []string, string) ([]byte, error)
}

// NFmt implements the Formatter inferface for $NYNERULES
type NFmt struct {
	ops           map[string]*Op
	menu          []string
	listener      event.Listener
	debug         bool
	extWithoutDot []string
}

// Fmt specifies formatting rules for a given extension
type Fmt struct {
	indent    int
	tabexpand bool
}

// Op specifies a formatting operation to be performed on an Acme buffer
type Op struct {
	Fmt Fmt
	Cmd []config.Command
}

// New constructs a Formatter that uses $NYNERULES for formatting
func New(conf *config.Config) Formatter {
	n := &NFmt{
		ops:           make(map[string]*Op),
		menu:          conf.Tag.Menu,
		listener:      event.NewListener(),
		debug:         len(os.Getenv("DEBUG")) > 0,
		extWithoutDot: []string{},
	}

	for _, spec := range conf.Format {
		for _, ext := range spec.Extensions {
			if !strings.Contains(ext, ".") {
				n.extWithoutDot = append(n.extWithoutDot, ext)
			}

			n.ops[ext] = &Op{
				Fmt: Fmt{
					indent:    spec.Indent,
					tabexpand: spec.Tabexpand,
				},
				Cmd: spec.Commands,
			}
		}
	}

	n.listener.RegisterNHook(event.WinHook{
		Handler: func(w *event.Win) {
			op, _ := n.getOp(w.File)
			if op != nil {
				n.SetupFormatting(w, op.Fmt)
			}
			err := n.WriteMenu(w)
			if err != nil {
				log.Println(err)
			}

		},
	})

	n.listener.RegisterPHook(event.EventHook{
		Handler: func(evt event.Event) event.Event {
			op, ext := n.getOp(evt.File)
			if op == nil {
				return evt
			}
			n.ExecCmds(evt, op.Cmd, ext)
			return evt
		},
	})

	km := &Keymap{
		GetWinFn: func(id int) (*event.Win, error) {
			l := n.listener.GetEventLoopByID(id)
			if l == nil {
				return nil, fmt.Errorf("could not find event loop")
			}
			return l.GetWin(), nil	
		},
		GetIndentFn: func(evt event.Event) int {
			op, _ := n.getOp(evt.File)
			if op == nil {
				return 8 // default
			}
			return op.Fmt.indent		
		},
	}

	// Tabexpand
	n.listener.RegisterKeyCmdHook(km.Tabexpand(func(evt event.Event) bool {
		op, _ := n.getOp(evt.File)
		if op == nil {
			return false
		}
		return op.Fmt.tabexpand		
	}))

	return n
}

func (n *NFmt) getOp(file string) (*Op, string) {
	ext := n.getExt(file, ".txt")
	op := n.ops[ext]
	return op, ext
}

// Run tells the Formatter to begin listening for Acme events
func (n *NFmt) Run() {
	log.Fatal(n.listener.Listen())
}

// ExecCmds executes commands that operate on stdin/stdout against the Acme buffer
// TODO: this should read the file once, create a unified diff, and apply the diff
//             to the buffer instead of doing so for each command
func (n *NFmt) ExecCmds(evt event.Event, commands []config.Command, ext string) error {
	updates := [][]byte{}
	for _, cmd := range commands {
		new, err := n.Refmt(evt, cmd.Exec, cmd.Args, ext)
		if err != nil {
			return err
		}
		updates = append(updates, new)
	}
	return n.WriteUpdates(evt, updates)
}

// WriteMenu writes the specified menu options to the Acme buffer
func (n *NFmt) WriteMenu(w *event.Win) error {
	if w == nil {
		return fmt.Errorf("state has drifted: *event.Win is nil")
	}
	if err := w.WriteToTag("\n"); err != nil {
		return err
	}
	for _, opt := range n.menu {
		cmd := fmt.Sprintf("  %s", opt)
		if err := w.WriteToTag(cmd); err != nil {
			return err
		}
	}
	return nil
}

// SetupFormatting opens the Acme buffer for writing and applies the indentation and
// tab expansion options provided in $NYNERULES
func (n *NFmt) SetupFormatting(w *event.Win, format Fmt) error {
	if w == nil {
		return fmt.Errorf("state has drifted: *event.Win is nil")
	}
	if format.indent == 0 {
		return nil
	}
	if err := w.WriteToTag("\n"); err != nil {
		return err
	}	
	if err := w.ExecInTag("Tab", strconv.Itoa(format.indent)); err != nil {
		return err
	}
	return nil
}

// Refmt executes a command to the Acme buffer and refreshes the buffer with updated contents
func (n *NFmt) Refmt(evt event.Event, x string, args []string, ext string) ([]byte, error) {
	l := n.listener.GetEventLoopByID(evt.ID)
	if l == nil {
		return []byte{}, fmt.Errorf("no event loop found")
	}
	old, err := l.GetWin().ReadBody()
	if err != nil {
		return []byte{}, err
	}
	tmp, err := ioutil.TempFile("", fmt.Sprintf("nyne%s", ext))     
	if err != nil {
		return []byte{}, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(old); err != nil {
		return []byte{}, err
	}
	if err := tmp.Close(); err != nil {
		return []byte{}, err
	}
	nargs := replaceName(args, tmp.Name())
	new, err := exec.Command(x, nargs...).CombinedOutput()
	if err != nil {
		return []byte{}, err
	}
	if bytes.Equal(old, new) {
		return old, nil
	}
	return new, nil
}

// WriteUpdates writes the updated contents to the file
func (n *NFmt) WriteUpdates(evt event.Event, updates [][]byte) error {
	l := n.listener.GetEventLoopByID(evt.ID)
	if l == nil {
		return fmt.Errorf("no event loop found")
	}
	w := l.GetWin()
	for _, update := range updates {
		if err := w.SetAddr(","); err != nil {
			return err
		}
		if err := w.SetData(update); err != nil {
			return err
		}
	}
	return nil
}

func replaceName(arr []string, name string) []string {
	newArr := make([]string, len(arr))
	for idx, str := range arr {
		if str == "$NAME" {
			newArr[idx] = name
		} else {
			newArr[idx] = arr[idx]
		}
	}
	return newArr
}

func (n *NFmt) getExt(in string, def string) string {
	filename := getFileName(in)
	if includes(filename, n.extWithoutDot) {
		return filename
	}
	pts := strings.Split(filename, ".")
	if len(pts) == len(in) {
		return def
	}
	return "." + pts[len(pts)-1]
}

func getFileName(in string) string {
	path := strings.Split(in, "/")
	return path[len(path)-1]
}

func includes(in string, dat []string) bool {
	for _, val := range dat {
		if val == in {
			return true
		}
	}
	return false
}
