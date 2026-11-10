package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/dusty/zoo/internal/backend"
	"github.com/dusty/zoo/internal/cage"
	"github.com/dusty/zoo/internal/task"
)

const taskUsage = `Usage:
  zoo task new NAME [DIRECTORY]       copy a project (default: current directory)
  zoo task run NAME [HARNESS] [-- ARGS...]
                                     run an agent; stop the task cage on exit
  zoo task list                      list saved tasks
  zoo task diff NAME [--stat]         review changes, including uncommitted edits
  zoo task apply NAME [--dry-run]     apply changes as unstaged host edits
  zoo task destroy NAME [--discard]  remove a task; refuse unapplied changes

Tasks exclude ignored files, secret masks, and Git metadata.
Each task starts with a separate Git baseline. Source history is not copied.
Review and apply require a stopped cage. Tasks survive exits and disconnects.
`

func cmdTask(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Print(taskUsage)
		return nil
	}
	action := args[0]
	if action == "list" {
		if len(args) != 1 {
			return fmt.Errorf("usage: zoo task list")
		}
		tasks, err := task.List()
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "TASK\tCAGE\tSOURCE")
		for _, t := range tasks {
			fmt.Fprintf(w, "%s\t%s\t%s\n", t.Name, t.Cage(), t.Source)
		}
		return w.Flush()
	}
	if len(args) < 2 {
		return fmt.Errorf("%s", taskUsage)
	}
	if action == "new" {
		if len(args) > 3 {
			return fmt.Errorf("usage: zoo task new NAME [DIRECTORY]")
		}
		source := "."
		if len(args) == 3 {
			source = args[2]
		}
		g, err := loadCfg()
		if err != nil {
			return err
		}
		// A subdirectory selects that directory, not its enclosing repository.
		t, err := task.Create(args[1], source, g)
		if err != nil {
			return err
		}
		fmt.Printf("Created task %s at %s\nRun: zoo task run %s\n", t.Name, t.Workdir(), t.Name)
		return nil
	}
	if action != "run" && action != "diff" && action != "apply" && action != "destroy" {
		return fmt.Errorf("unknown task command %q\n%s", action, taskUsage)
	}
	t, err := task.Load(args[1])
	if err != nil {
		return err
	}
	unlock, err := t.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	// Load again after taking the lock: another command may have advanced
	// the baseline between the initial read and acquiring the lock.
	t, err = task.Load(args[1])
	if err != nil {
		return err
	}
	if action == "run" {
		return runTask(&t, args[2:])
	}
	fs := flag.NewFlagSet("task "+action, flag.ContinueOnError)
	var stat, dryRun, discard bool
	switch action {
	case "diff":
		fs.BoolVar(&stat, "stat", false, "show a summary")
	case "apply":
		fs.BoolVar(&dryRun, "dry-run", false, "check without changing host files")
	case "destroy":
		fs.BoolVar(&discard, "discard", false, "discard unapplied changes")
	}
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	b, err := taskBackend(t)
	if err != nil {
		return err
	}
	if b != nil {
		st, exists, err := b.Get(context.Background(), cage.Prefix+t.Cage())
		if err != nil {
			return fmt.Errorf("check task cage: %w", err)
		}
		if exists && st.Running {
			return fmt.Errorf("task cage is running; stop it with `zoo stop %s --backend %s` before %s", t.Cage(), t.Backend, action)
		}
	}
	switch action {
	case "diff":
		patch, err := t.Diff(stat)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(patch)
		return err
	case "apply":
		n, err := t.Apply(dryRun)
		if err != nil {
			return err
		}
		if dryRun {
			fmt.Printf("%d changed file(s) can be applied to %s\n", n, t.Source)
		} else {
			fmt.Printf("Applied %d changed file(s) to %s; edits are unstaged\n", n, t.Source)
		}
	case "destroy":
		// Check for unapplied work before removing the runtime instance.
		if !discard {
			patch, err := t.Diff(false)
			if err != nil {
				return err
			}
			if len(patch) > 0 {
				return fmt.Errorf("task has unapplied changes; apply them first, or use --discard")
			}
		}
		if b != nil {
			if _, exists, err := b.Get(context.Background(), cage.Prefix+t.Cage()); err != nil {
				return err
			} else if exists {
				if err := b.Delete(context.Background(), cage.Prefix+t.Cage()); err != nil {
					return err
				}
			}
		}
		if err := t.Destroy(discard); err != nil {
			return err
		}
		fmt.Printf("Destroyed task %s\n", t.Name)
	}
	return nil
}

// Inspection uses the task's saved backend, never whichever backend is now
// selected globally. If we cannot check its state, do not touch host files.
func taskBackend(t task.Task) (backend.Backend, error) {
	if t.Backend == "" {
		return nil, nil
	}
	g, err := loadCfg()
	if err != nil {
		return nil, err
	}
	b, err := backend.New(t.Backend, backendOptions(g))
	if err != nil {
		return nil, err
	}
	if info := b.Detect(context.Background()); !info.Ready() {
		return nil, fmt.Errorf("task backend %s is unavailable; start it before reviewing or applying this task", t.Backend)
	}
	return b, nil
}

func runTask(t *task.Task, args []string) (err error) {
	o, err := parseRun(args)
	if err != nil {
		return err
	}
	if o.harness == "" {
		o.harness = "claude"
	}
	g, err := loadCfg()
	if err != nil {
		return err
	}
	e, err := resolve(context.Background(), g)
	if err != nil {
		return err
	}
	if t.Backend != "" && t.Backend != e.b.Name() {
		return fmt.Errorf("task uses %s; run with --backend %s", t.Backend, t.Backend)
	}
	t.Backend = e.b.Name()
	if err := t.Save(); err != nil {
		return err
	}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := os.Chdir(t.Workdir()); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.Chdir(wd)) }()
	defer func() {
		ctx := context.Background()
		id := cage.Prefix + t.Cage()
		st, exists, getErr := e.b.Get(ctx, id)
		if getErr != nil {
			err = errors.Join(err, getErr)
			return
		}
		if exists && st.Running {
			err = errors.Join(err, e.b.Stop(ctx, id))
		}
	}()
	return runIn(o, "")
}
