// Command fanout is a terminal client for a fanoutd server. It speaks HTTP and
// nothing else — it does not link the sqlite driver, so it physically cannot
// open a local database no matter what directory it is run from.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"fanoutd/internal/client"
)

const (
	exitOK = 0
	// exitFailure is a client or server problem: bad usage, unreachable
	// server, missing task.
	exitFailure = 1
	// exitTaskError means the command worked and the task itself ended in
	// error. Keeping it distinct is what lets `fanout watch` drive a shell script.
	exitTaskError = 2
)

// errTaskFailed maps onto exitTaskError. It carries no message of its own; the
// command has already printed the task's error.
var errTaskFailed = errors.New("task ended in error")

type env struct {
	ctx    context.Context
	client *client.Client
	cfg    settings
	out    io.Writer
}

type command struct {
	name    string
	summary string
	run     func(e *env, args []string) error
}

func commands() []command {
	return []command{
		{"add", "create a task, optionally starting it", cmdAdd},
		{"breakdown", "split an idea into subtasks and run them", cmdBreakdown},
		{"plan", "the wave plan of a breakdown, and its subtasks", cmdPlan},
		{"ls", "list tasks", cmdLs},
		{"show", "task detail plus its last few steps", cmdShow},
		{"watch", "follow a running task until it finishes", cmdWatch},
		{"trace", "step history, truncated unless --full", cmdTrace},
		{"start", "start or resume the agent loop", cmdStart},
		{"stop", "cancel the running agent loop", cmdStop},
		{"mv", "move a task to ideas, todo, or finished", cmdMv},
		{"rm", "delete a task and its workspace", cmdRm},
		{"files", "list the files a task produced", cmdFiles},
		{"cat", "print one workspace file", cmdCat},
		{"continue", "new goal against the same workspace", cmdContinue},
		{"retry", "re-run the same brief in a clean workspace", cmdRetry},
		{"models", "models this server will accept", cmdModels},
	}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout))
}

func run(args []string, out io.Writer) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return exitFailure
	}

	name, rest := args[0], args[1:]
	switch name {
	case "help", "-h", "--help":
		usage(os.Stdout)
		return exitOK
	}

	var cmd *command
	for _, c := range commands() {
		if c.name == name {
			cmd = &c
			break
		}
	}
	if cmd == nil {
		fmt.Fprintf(os.Stderr, "fanout: unknown command %q\n\n", name)
		usage(os.Stderr)
		return exitFailure
	}

	// Ctrl-C during a watch should end the poll loop, not kill the run on the
	// server; the loop keeps going and `fanout watch` can pick it back up.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	e := &env{ctx: ctx, out: out}
	if err := cmd.run(e, rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitFailure
		}
		if errors.Is(err, errTaskFailed) {
			return exitTaskError
		}
		if errors.Is(err, context.Canceled) {
			return exitFailure
		}
		fmt.Fprintf(os.Stderr, "fanout: %s\n", err)
		return exitFailure
	}
	return exitOK
}

// flags builds a command's flag set with the two global flags already on it, so
// `--server` works after any subcommand rather than only before one.
func (e *env) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("fanout "+name, flag.ContinueOnError)
	fs.String("server", "", "server URL (default $FANOUT_URL, config file, "+defaultServer+")")
	fs.String("token", "", "API token (default $FANOUT_TOKEN or config file)")
	return fs
}

// dial resolves settings from the parsed flags and opens the client. Every
// command calls it after parsing.
func (e *env) dial(fs *flag.FlagSet) {
	server := fs.Lookup("server").Value.String()
	token := fs.Lookup("token").Value.String()
	e.cfg = resolveSettings(server, token)
	e.client = client.New(e.cfg.server, e.cfg.token)
}

// parse runs the flag set and connects. It exists so no command forgets to dial.
func (e *env) parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(permute(fs, args)); err != nil {
		return err
	}
	e.dial(fs)
	return nil
}

// permute moves flags ahead of positional arguments. Go's flag package stops
// parsing at the first non-flag, which would make `fanout trace c762 --full` read
// --full as a second positional and silently ignore it — the id comes first in
// every one of these commands, so that is the form people will type.
func permute(fs *flag.FlagSet, args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			rest = append(rest, args[i+1:]...)
			break
		}
		if len(arg) < 2 || arg[0] != '-' {
			rest = append(rest, arg)
			continue
		}

		flags = append(flags, arg)
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if hasValue {
			continue // --flag=value carries its own argument
		}
		// Only a flag that takes a value may claim the next argument, and only
		// one this command actually defines; an unknown flag is left for Parse
		// to reject rather than silently eating a positional.
		if def := fs.Lookup(name); def != nil && !isBoolFlag(def) && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, rest...)
}

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// resolve is parse plus the leading task-id argument, which most commands take.
func (e *env) resolve(fs *flag.FlagSet, args []string) (string, error) {
	if err := e.parse(fs, args); err != nil {
		return "", err
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return "", fmt.Errorf("a task id or prefix is required")
	}
	return resolveID(e.ctx, e.client, fs.Arg(0))
}

// describeErr adds the one piece of context the server cannot: which address we
// were talking to, and whether a token was in play.
func (e *env) describeErr(err error) error {
	var apiErr *client.Error
	if errors.As(err, &apiErr) && apiErr.Unauthorized() {
		if e.cfg.token == "" {
			return fmt.Errorf("%s requires a token — set FANOUT_TOKEN or pass --token", e.cfg.server)
		}
		return fmt.Errorf("%s rejected the token", e.cfg.server)
	}
	return err
}

func usage(w io.Writer) {
	fmt.Fprint(w, `fanout — fanoutd CLI

usage: fanout <command> [flags] [args]

commands:
`)
	for _, c := range commands() {
		fmt.Fprintf(w, "  %-10s %s\n", c.name, c.summary)
	}
	fmt.Fprintf(w, `
Every command accepts --server and --token. Server resolution is
--server, then $FANOUT_URL, then the config file, then %s.
The config file is $XDG_CONFIG_HOME/fanoutd/config.toml (or
~/.config/fanoutd/config.toml):

  url = "https://board.example"
  token = "..."

Task ids may be given as any unambiguous prefix, or as part of a title.
Group ids, for breakdown and plan, resolve the same way.
`, defaultServer)
	fmt.Fprint(w, `
exit codes: 0 ok, 1 command failed, 2 the task itself ended in error.

  fanout <command> --help    flags for one command
`)
}

// runState is aliased so command code is not littered with the package name.
type runState = client.RunState

func clientNewTask(title, desc, goal, model string) client.NewTask {
	return client.NewTask{Title: title, Description: desc, Goal: goal, Model: model}
}

// followup builds a continue or retry body. Model is a pointer server-side
// because "" is a real value there — it means fall back to the configured
// default — so an unset --model must be omitted rather than sent as empty,
// which is what makes the new task inherit the old one's model.
func followup(title, desc, goal, model string, start bool) client.Followup {
	f := client.Followup{Title: title, Description: desc, Goal: goal, Start: start}
	if model != "" {
		f.Model = &model
	}
	return f
}

// readStdin backs `--goal -`, so a long brief can come from a file or a pipe
// instead of being wrestled onto a command line.
func readStdin() (string, error) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
