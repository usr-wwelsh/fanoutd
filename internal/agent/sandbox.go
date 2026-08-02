package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// The sandbox is the security boundary, so the command string is never parsed,
// escaped, or matched against an allowlist: it goes to /bin/sh inside the jail,
// where there is nothing to escape to. That is also what keeps fanoutd
// language-agnostic — whatever toolchain the host has under /usr is what the
// agent can run, with no per-language code here.
//
// bubblewrap is mandatory. When the probe fails the sandbox is not created, the
// tool is never advertised to the model, and Exec refuses it.

// execOutputBytes caps what one command may return. Tool results are stored in
// the trace and replayed into the next prompt, so an uncapped `find /` costs
// context budget and tokens rather than just scrollback.
const execOutputBytes = 6000

// execHeadBytes is how much of a capped result comes from the start. The tail
// holds the error summary of most build and test runs, so it keeps the rest.
const execHeadBytes = 4000

const defaultExecTimeout = 120 * time.Second

// cacheSubdirs are shared across tasks and mounted at /cache. Every one of them
// is either content-addressed or takes its own lock, so concurrent builds are
// safe — and sharing them is what keeps a second build from starting cold.
var cacheSubdirs = []string{"go-build", "go-mod", "cargo", "rustup", "npm", "xdg"}

// buildSubdirs are private to one task and mounted at /build. Cargo takes an
// exclusive lock on its target directory, so a shared one would serialize
// parallel subtasks into what looks like a hang.
var buildSubdirs = []string{"home", "go", "cargo-target"}

// SandboxConfig is the operator's half of the sandbox: what it may reach and
// what it may consume.
type SandboxConfig struct {
	// Network leaves the sandbox on the host network. Off by default: a command
	// with no network cannot exfiltrate the workspace or the API key.
	Network bool
	// Timeout bounds one command. It starts after a slot is acquired, not before,
	// so queueing never eats a command's budget.
	Timeout time.Duration
	// MemoryMax, TasksMax and CPUQuota are systemd scope properties. bubblewrap
	// has no resource limits of its own — --unshare-pid contains a fork bomb's
	// cleanup but not its appetite.
	MemoryMax string
	TasksMax  int
	CPUQuota  string
	// MaxExec bounds concurrent commands across all tasks. Zero is unlimited,
	// which is the default: the kernel arbitrates through the cgroup limits above,
	// and serializing here would make the rolling-parallel scheduler run one wide
	// at exactly its most expensive step.
	MaxExec int
	// ROBind lists extra host paths to mount read-only, for toolchains installed
	// outside /usr — rustup under ~/.cargo, nvm, pyenv, go install. Empty by
	// default because binding a home directory would expose ssh keys and env
	// files to every agent. Any entry with a bin/ subdirectory joins PATH.
	ROBind []string
	// StateDir holds the shared cache and the per-task build directories.
	StateDir string
}

// Sandbox runs shell commands under bubblewrap.
type Sandbox struct {
	bwrap      string
	systemdRun string // "" when there is no user manager to place a scope in
	cfg        SandboxConfig
	cacheDir   string
	buildDir   string
	path       string
	slots      chan struct{} // nil when unlimited
}

// NewSandbox probes bubblewrap by running a real command through the real jail,
// not by looking for the binary: an unprivileged user namespace can be missing
// or blocked, and finding that out at startup is what lets the tool be withheld
// instead of failing on first use.
func NewSandbox(cfg SandboxConfig) (*Sandbox, error) {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("bubblewrap not found: %w", err)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultExecTimeout
	}

	s := &Sandbox{
		bwrap:    bwrap,
		cfg:      cfg,
		cacheDir: filepath.Join(cfg.StateDir, "cache"),
		buildDir: filepath.Join(cfg.StateDir, "build"),
		path:     execPath(cfg.ROBind),
	}
	if cfg.MaxExec > 0 {
		s.slots = make(chan struct{}, cfg.MaxExec)
	}
	if path, err := exec.LookPath("systemd-run"); err == nil && os.Getenv("XDG_RUNTIME_DIR") != "" {
		s.systemdRun = path
	}
	// The cache directories are created here rather than left to each toolchain:
	// some create their own and some fail outright, and the difference would
	// surface as a confusing build error rather than a missing directory.
	for _, dir := range append(cacheSubdirs, "") {
		if err := os.MkdirAll(filepath.Join(s.cacheDir, dir), 0o755); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(s.buildDir, 0o755); err != nil {
		return nil, err
	}
	if err := s.probe(); err != nil {
		return nil, err
	}
	return s, nil
}

// probe runs a trivial command through the full sandbox. A failure here means
// the kernel will not give us the namespaces, so the sandbox is unusable.
func (s *Sandbox) probe() error {
	dir, err := os.MkdirTemp("", "fanout-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, s.bwrap, append(s.jailArgs(dir, dir), "/bin/sh", "-c", "exit 0")...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bubblewrap probe failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	// The scope is a resource limit, not a boundary, so losing it degrades the
	// sandbox rather than disabling it.
	if s.systemdRun != "" {
		if err := exec.CommandContext(ctx, s.systemdRun, append(s.scopeArgs(), "/bin/true")...).Run(); err != nil {
			s.systemdRun = ""
		}
	}
	return nil
}

// Describe reports what the sandbox will enforce, for the startup log.
func (s *Sandbox) Describe() string {
	parts := []string{"bubblewrap"}
	if s.cfg.Network {
		parts = append(parts, "network ON")
	} else {
		parts = append(parts, "no network")
	}
	if s.systemdRun != "" {
		parts = append(parts, fmt.Sprintf("mem %s, tasks %d, cpu %s", s.cfg.MemoryMax, s.cfg.TasksMax, s.cfg.CPUQuota))
	} else {
		parts = append(parts, "no cgroup limits (systemd user scope unavailable)")
	}
	parts = append(parts, fmt.Sprintf("timeout %s", s.cfg.Timeout))
	if s.cfg.MaxExec > 0 {
		parts = append(parts, fmt.Sprintf("max %d concurrent", s.cfg.MaxExec))
	}
	if len(s.cfg.ROBind) > 0 {
		parts = append(parts, "ro: "+strings.Join(s.cfg.ROBind, ","))
	}
	return strings.Join(parts, ", ")
}

// execPath is PATH inside the jail: the host's system directories, plus a bin
// subdirectory from any extra read-only bind, which is where rustup, nvm and
// `go install` put their toolchains.
func execPath(roBind []string) string {
	dirs := []string{"/usr/local/bin", "/usr/bin", "/bin", "/usr/local/sbin", "/usr/sbin", "/sbin"}
	for _, b := range roBind {
		bin := filepath.Join(b, "bin")
		if info, err := os.Stat(bin); err == nil && info.IsDir() {
			dirs = append([]string{bin}, dirs...)
		}
	}
	return strings.Join(dirs, ":")
}

// rootBinds mirrors the host's system directories read-only, following the
// merged-usr symlinks on hosts that have them and binding the real directories
// on hosts that do not.
func rootBinds() []string {
	args := []string{"--ro-bind", "/usr", "/usr"}
	for _, dir := range []string{"/bin", "/sbin", "/lib", "/lib64", "/lib32"} {
		info, err := os.Lstat(dir)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(dir)
			if err != nil {
				continue
			}
			args = append(args, "--symlink", target, dir)
			continue
		}
		args = append(args, "--ro-bind", dir, dir)
	}
	// /etc carries the ssl trust store and ld.so config that toolchains need.
	return append(args, "--ro-bind-try", "/etc", "/etc", "--ro-bind-try", "/opt", "/opt")
}

// jailArgs builds the bubblewrap invocation. --clearenv is load-bearing: without
// it a command reads OPENROUTER_API_KEY out of the environment and it lands in
// the trace, and from there back into the next prompt.
func (s *Sandbox) jailArgs(workDir, buildDir string) []string {
	args := []string{"--unshare-all"}
	if s.cfg.Network {
		args = append(args, "--share-net")
	}
	args = append(args,
		"--die-with-parent",
		"--new-session",
		"--clearenv",
	)
	args = append(args, rootBinds()...)
	args = append(args,
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
	)
	for _, b := range s.cfg.ROBind {
		args = append(args, "--ro-bind-try", b, b)
	}
	// The workspace is bound at the same absolute path it has on the host, not at
	// a tidy /work. The model is told that path in its prompt and the file tools
	// accept it, so a shell that disagreed would make the prompt a lie for one
	// tool out of six — and the failure was silent: `mkdir -p <path> && cd <path>
	// && go mod init` creates the missing path inside the jail's own root, works
	// perfectly, and evaporates when the command exits.
	args = append(args,
		"--bind", workDir, workDir,
		"--bind", buildDir, "/build",
		"--bind", s.cacheDir, "/cache",
		"--chdir", workDir,
	)

	// Build output goes to a per-task directory and caches to a shared one, so
	// parallel subtasks never contend over a target directory — cargo takes an
	// exclusive lock on its own, which would serialize them into a stall that
	// looks like a hang. The shared caches are all either content-addressed or
	// self-locking, so concurrent use is safe.
	env := [][2]string{
		{"PATH", s.path},
		{"HOME", "/build/home"},
		{"TMPDIR", "/tmp"},
		{"LANG", "C.UTF-8"},
		{"LC_ALL", "C.UTF-8"},
		{"TERM", "dumb"},
		{"CI", "1"},
		{"GOFLAGS", "-mod=mod"},
		{"GOCACHE", "/cache/go-build"},
		{"GOMODCACHE", "/cache/go-mod"},
		{"GOPATH", "/build/go"},
		{"CARGO_HOME", "/cache/cargo"},
		{"CARGO_TARGET_DIR", "/build/cargo-target"},
		{"RUSTUP_HOME", "/cache/rustup"},
		{"npm_config_cache", "/cache/npm"},
		{"XDG_CACHE_HOME", "/cache/xdg"},
	}
	if !s.cfg.Network {
		// Saves every package manager a DNS timeout before it reports the real
		// problem, which would otherwise eat most of the command budget.
		env = append(env, [2]string{"GOPROXY", "off"}, [2]string{"CARGO_NET_OFFLINE", "true"})
	}
	for _, kv := range env {
		args = append(args, "--setenv", kv[0], kv[1])
	}
	return args
}

// scopeArgs places the command in a transient systemd scope. --scope runs it as
// a child of systemd-run, which is a child of us, so --die-with-parent still
// reaches the jail and cancelling the run still tears the tree down.
func (s *Sandbox) scopeArgs() []string {
	args := []string{"--user", "--scope", "--quiet", "--collect"}
	if s.cfg.MemoryMax != "" {
		args = append(args, "-p", "MemoryMax="+s.cfg.MemoryMax)
	}
	if s.cfg.TasksMax > 0 {
		args = append(args, "-p", fmt.Sprintf("TasksMax=%d", s.cfg.TasksMax))
	}
	if s.cfg.CPUQuota != "" {
		args = append(args, "-p", "CPUQuota="+s.cfg.CPUQuota)
	}
	return args
}

// taskBuildDir is a task's private build scratch, mounted at /build. It sits
// outside the workspace so build artifacts never become deliverables and never
// collide with a sibling's claims.
func (s *Sandbox) taskBuildDir(taskID string) (string, error) {
	if taskID == "" {
		taskID = "solo"
	}
	dir := filepath.Join(s.buildDir, taskID)
	for _, sub := range buildSubdirs {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// DiscardTask removes a task's private build directory. Nothing in there is a
// deliverable — it is compiler scratch and a fake HOME — so it goes with the
// task rather than with the workspace, which is shared and may outlive it.
func (s *Sandbox) DiscardTask(taskID string) error {
	if strings.TrimSpace(taskID) == "" {
		return nil
	}
	dir := filepath.Join(s.buildDir, taskID)
	// A task id is a hex string from the store, but this is a recursive delete
	// keyed on it, so it is checked rather than trusted.
	if filepath.Dir(dir) != s.buildDir || filepath.Base(dir) != taskID {
		return fmt.Errorf("refusing to remove build directory for %q", taskID)
	}
	return os.RemoveAll(dir)
}

// ReapBuildDirs removes build directories belonging to tasks that no longer
// exist, and reports how many it dropped. Deleting a task now takes its scratch
// with it, but nothing collected what earlier versions left behind, and a
// process that was killed mid-run never got to delete anything.
func (s *Sandbox) ReapBuildDirs(live map[string]bool) (int, error) {
	// A nil set means the caller could not find out which tasks exist, which is
	// not the same as there being none. An empty board is an empty map.
	if live == nil {
		return 0, nil
	}
	entries, err := os.ReadDir(s.buildDir)
	if err != nil {
		return 0, err
	}
	dropped := 0
	for _, e := range entries {
		if !e.IsDir() || live[e.Name()] {
			continue
		}
		if err := s.DiscardTask(e.Name()); err != nil {
			return dropped, err
		}
		dropped++
	}
	return dropped, nil
}

// Run executes command in workDir and returns its combined output. A command
// that fails is not an error: a failing build's output is the useful part, so
// only being unable to run at all returns one.
func (s *Sandbox) Run(ctx context.Context, workDir, taskID, command string) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("command is required")
	}
	buildDir, err := s.taskBuildDir(taskID)
	if err != nil {
		return "", err
	}
	if err := s.acquire(ctx); err != nil {
		return "", err
	}
	defer s.release()

	// The timeout starts here, after the wait: a command that queued for four
	// minutes must not then fail for reasons that have nothing to do with it.
	runCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	name, args := s.bwrap, append(s.jailArgs(workDir, buildDir), "/bin/sh", "-c", command)
	if s.systemdRun != "" {
		name, args = s.systemdRun, append(s.scopeArgs(), append([]string{s.bwrap}, args...)...)
	}

	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, runErr := runWithDeadline(runCtx, cmd)

	result := capOutput(string(out))
	switch {
	case runCtx.Err() == context.DeadlineExceeded:
		return result + fmt.Sprintf("\n\n[killed: exceeded the %s time limit]", s.cfg.Timeout), nil
	case ctx.Err() != nil:
		return "", ctx.Err()
	case runErr != nil:
		var exit *exec.ExitError
		if errors.As(runErr, &exit) {
			return result + fmt.Sprintf("\n\n[exit status %d]", exit.ExitCode()), nil
		}
		return "", runErr
	}
	if strings.TrimSpace(result) == "" {
		return "[no output, exit status 0]", nil
	}
	return result + "\n\n[exit status 0]", nil
}

// runWithDeadline runs cmd and kills its whole process group when ctx ends.
// exec.CommandContext would signal only the direct child, leaving the shell's
// own children behind; --die-with-parent and the pid namespace cover the rest,
// but the group kill is what makes the timeout immediate.
func runWithDeadline(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	// One buffer for both streams: os/exec gives them a single descriptor when
	// the writers are identical, so there is no concurrent write to guard, and
	// interleaved output matches what a terminal would have shown.
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return buf.Bytes(), err
	case <-ctx.Done():
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return buf.Bytes(), nil
	}
}

func (s *Sandbox) acquire(ctx context.Context) error {
	if s.slots == nil {
		return nil
	}
	select {
	case s.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		// A stopped run must not sit in the queue holding a scheduler slot.
		return ctx.Err()
	}
}

func (s *Sandbox) release() {
	if s.slots != nil {
		<-s.slots
	}
}

// capOutput keeps the head and the tail. Build and test runners put the command
// line at the top and the failure summary at the bottom, and dropping either
// leaves the model guessing.
func capOutput(out string) string {
	if len(out) <= execOutputBytes {
		return out
	}
	tail := execOutputBytes - execHeadBytes
	return fmt.Sprintf("%s\n\n[... %d bytes omitted ...]\n\n%s",
		out[:execHeadBytes], len(out)-execOutputBytes, out[len(out)-tail:])
}
