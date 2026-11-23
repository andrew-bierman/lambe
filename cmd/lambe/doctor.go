package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/gate"
	"github.com/andrew-bierman/lambe/internal/surface/rex"
	"github.com/andrew-bierman/lambe/internal/symphony"
)

// Check outcomes. Only fail makes `lambe doctor` exit non-zero.
const (
	statusOK   = "ok"
	statusWarn = "warn"
	statusFail = "fail"
	statusSkip = "skip"
)

// errDoctorFailed is returned when at least one check failed; the report
// itself is the message, so main exits 1 without logging it again.
var errDoctorFailed = errors.New("doctor found problems")

// checkResult is one line of the doctor report.
type checkResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// doctorEnv holds the host seams doctor reads, so tests can run against
// temp dirs and fake commands.
type doctorEnv struct {
	sys      sysEnv
	stateDir string
	now      func() time.Time
	lookPath func(string) (string, error)
	getenv   func(string) string
	// rexRunner probes the rex CLI.
	rexRunner rex.CommandRunner
	// hostname resolves the machine name; nil uses os.Hostname.
	hostname func() (string, error)
	// timeBD runs one read-only bd command in dir and reports how long it
	// took; nil skips the latency check.
	timeBD func(ctx context.Context, bin, dir string, args ...string) (time.Duration, error)
}

func hostDoctorEnv() doctorEnv {
	return doctorEnv{
		sys:       hostEnv(),
		stateDir:  daemon.DefaultStateDir(),
		now:       time.Now,
		lookPath:  exec.LookPath,
		getenv:    os.Getenv,
		rexRunner: rexExec,
		hostname:  os.Hostname,
		timeBD:    timeBDCommand,
	}
}

// timeBDCommand runs bd read-only in dir and returns the wall time.
func timeBDCommand(ctx context.Context, bin, dir string, args ...string) (time.Duration, error) {
	cmd := exec.CommandContext(ctx, bin, args...) // #nosec G204 -- fixed read-only bd probe argv
	cmd.Dir = dir
	start := time.Now()
	err := cmd.Run()
	return time.Since(start), err
}

func rexExec(ctx context.Context, name string, args ...string) (string, int, error) {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- fixed rex probe argv
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return string(out), exitErr.ExitCode(), err
	}
	return string(out), 0, err
}

// runDoctor implements `lambe doctor`: it checks everything that can make
// agents silently stop and prints what to do about each problem.
func runDoctor(args []string, e doctorEnv, out io.Writer) error {
	fs := flag.NewFlagSet("lambe doctor", flag.ContinueOnError)
	configFile := fs.String("config", "", "path to a lambe config file")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	label := fs.String("service-label", "", "check the service installed with `lambe service install -label NAME`")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	e.sys.Label = *label
	results := diagnose(context.Background(), e, *configFile)

	failed := false
	counts := map[string]int{}
	for _, r := range results {
		counts[r.Status]++
		failed = failed || r.Status == statusFail
	}
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			return err
		}
	} else {
		say(out, "lambe doctor\n\n")
		for _, r := range results {
			say(out, "  %-4s  %-10s %s\n", r.Status, r.Name, r.Detail)
			if r.Fix != "" {
				say(out, "                   fix: %s\n", r.Fix)
			}
		}
		say(out, "\n%d ok, %d warn, %d fail\n", counts[statusOK], counts[statusWarn], counts[statusFail])
	}
	if failed {
		return errDoctorFailed
	}
	return nil
}

// diagnose runs every check in dependency order. A config that cannot load
// ends the run early: everything else is derived from it.
func diagnose(ctx context.Context, e doctorEnv, configFile string) []checkResult {
	var rs []checkResult
	add := func(name, status, detail, fix string) {
		rs = append(rs, checkResult{Name: name, Status: status, Detail: detail, Fix: fix})
	}

	path, err := cliConfigPath(configFile)
	if err == nil && path == "" {
		err = fmt.Errorf("no config at %s", config.DefaultPath())
	}
	var cfg *config.Config
	if err == nil {
		cfg, err = config.Load(config.Options{File: path})
	}
	if err != nil {
		add("config", statusFail, err.Error(), "run `lambe init` (or pass -config FILE)")
		return rs
	}
	add("config", statusOK, fmt.Sprintf("%s (harness %s)", path, cmpOr(cfg.Harness, "pi")), "")

	rs = append(rs, checkWorkflow(cfg))
	rs = append(rs, checkTracker(cfg)...)
	bdResult := checkBD(ctx, e)
	rs = append(rs, bdResult)
	if bdResult.Status == statusOK {
		rs = append(rs, checkBDLatency(ctx, e, cfg)...)
	}
	rs = append(rs, checkHarness(cfg))
	rs = append(rs, checkIdentity(e, cfg))
	rs = append(rs, checkDaemon(e, cfg)...)
	rs = append(rs, checkOrphanClaims(e, cfg)...)
	rs = append(rs, checkService(ctx, e, path))
	rs = append(rs, checkStateAPI(ctx, e, cfg))
	rs = append(rs, checkRex(ctx, e))
	return rs
}

// genericHostname matches names many machines share ("localhost",
// "MacBook-Pro", cloud "ip-10-0-0-1"): two hosts using one as their actor
// would both "win" the same claim.
var genericHostname = regexp.MustCompile(`(?i)^(localhost|unknown|host|server|desktop|laptop|ubuntu|debian|fedora|raspberrypi|imac|macbook([-_ ]?(air|pro))?|mac([-_ ]?(mini|studio|pro))?|ip-\d.*)$`)

// checkIdentity prints the actor this daemon claims as and warns about the
// two identity mistakes that break multi-host setups: a generic machine
// name, and a bd node_id set against a shared server.
func checkIdentity(e doctorEnv, cfg *config.Config) checkResult {
	host := e.hostname
	if host == nil {
		host = os.Hostname
	}
	machine, actor := cfg.Identity(host)
	r := checkResult{Name: "identity", Status: statusOK, Detail: fmt.Sprintf("machine %s, actor %s", machine, actor)}
	var warns []string
	// An explicit actor already keeps claims distinct; only an actor derived
	// from the machine name inherits a generic name's collision risk.
	if genericHostname.MatchString(machine) && cfg.Actor == "" {
		warns = append(warns, fmt.Sprintf("machine name %q is generic and likely collides with another host", machine))
		r.Fix = "set a distinct `machine:` (or LAMBE_MACHINE) in the config on every host"
	}
	if id := beadsNodeID(e); id != "" && sharedServer(cfg) {
		warns = append(warns, fmt.Sprintf("bd node_id %q is set but the tracker uses a shared server: hosts are one replica, and a per-host node_id makes `bd reclaim` skip other hosts' leases", id))
		r.Fix = strings.TrimSpace(r.Fix + " Unset node_id (BEADS_NODE_ID, ~/.config/bd/config.yaml) or use one value on every host.")
	}
	if len(warns) > 0 {
		r.Status = statusWarn
		r.Detail += "; " + strings.Join(warns, "; ")
	}
	return r
}

// sharedServer reports whether the primary tracker's bd database is served
// by a dolt sql-server rather than embedded in the directory.
func sharedServer(cfg *config.Config) bool {
	if len(cfg.Trackers) == 0 {
		return false
	}
	data, err := os.ReadFile(filepath.Join(cfg.Trackers[0].Path, ".beads", "metadata.json")) // #nosec G304 -- configured tracker directory
	if err != nil {
		return false
	}
	var meta struct {
		Mode string `json:"dolt_mode"`
	}
	return json.Unmarshal(data, &meta) == nil && meta.Mode != "" && meta.Mode != "embedded"
}

// beadsNodeID returns the bd node_id from BEADS_NODE_ID or the per-machine
// bd config file, empty when unset.
func beadsNodeID(e doctorEnv) string {
	if v := strings.TrimSpace(e.getenv("BEADS_NODE_ID")); v != "" {
		return v
	}
	data, err := os.ReadFile(filepath.Join(e.sys.Home, ".config", "bd", "config.yaml")) // #nosec G304 -- fixed per-user bd config path
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "node_id:"); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

func cmpOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func checkWorkflow(cfg *config.Config) checkResult {
	r := checkResult{Name: "workflow"}
	data, err := os.ReadFile(cfg.WorkflowPath)
	if err == nil {
		_, err = symphony.ParseWorkflow(data, symphony.OSEnv)
	}
	if err != nil {
		r.Status, r.Detail = statusFail, err.Error()
		r.Fix = "fix " + cfg.WorkflowPath + "; `lambe init` writes a starter WORKFLOW.md"
		return r
	}
	r.Status, r.Detail = statusOK, cfg.WorkflowPath
	return r
}

// checkTracker verifies the tracker directory and that completion gates
// exist: without a gate the daemon refuses to start.
func checkTracker(cfg *config.Config) []checkResult {
	if len(cfg.Trackers) == 0 {
		return []checkResult{{Name: "tracker", Status: statusFail, Detail: "no trackers configured", Fix: "add a trackers entry to the config"}}
	}
	tr := cfg.Trackers[0]
	if _, err := os.Stat(filepath.Join(tr.Path, ".beads")); err != nil {
		return []checkResult{{Name: "tracker", Status: statusFail, Detail: fmt.Sprintf("%s: %v", tr.Name, err), Fix: "run `bd init` in " + tr.Path}}
	}
	rs := []checkResult{{Name: "tracker", Status: statusOK, Detail: fmt.Sprintf("%s (%s) at %s", tr.Name, tr.Type, tr.Path)}}
	gates := checkResult{Name: "gates"}
	if err := requireGates(cfg); err != nil {
		gates.Status, gates.Detail = statusFail, err.Error()
		gates.Fix = "add a [gate] table to a .beads/formulas/*.formula.toml"
	} else {
		specs, _ := gate.LoadDir(filepath.Join(tr.Path, ".beads", "formulas"))
		gates.Status, gates.Detail = statusOK, fmt.Sprintf("%d completion gate(s) in the tracker formulas", len(specs))
	}
	return append(rs, gates)
}

func checkBD(ctx context.Context, e doctorEnv) checkResult {
	r := checkResult{Name: "bd"}
	bd, err := e.lookPath("bd")
	if err != nil {
		r.Status, r.Detail = statusFail, "bd not found on PATH"
		r.Fix = "install Beads (https://github.com/gastownhall/beads); a service needs it on the unit's PATH too"
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ver, err := e.sys.Run(ctx, bd, "--version")
	if err != nil {
		r.Status, r.Detail = statusFail, fmt.Sprintf("%s --version failed: %v", bd, err)
		r.Fix = "reinstall bd"
		return r
	}
	r.Status, r.Detail = statusOK, strings.TrimSpace(ver)+" at "+bd
	return r
}

// slowBDThreshold is the single-call latency above which an embedded hub is
// slow enough to starve the daemon: every tick fans out several bd calls and
// they serialize behind one writer lock.
const slowBDThreshold = 5 * time.Second

// checkBDLatency times one trivial read and one metadata query on an
// embedded tracker (the metadata scan is the expensive query attempt records
// used to depend on) and warns when either is slow, recommending Dolt server
// mode. Server-mode and unreadable trackers are skipped.
func checkBDLatency(ctx context.Context, e doctorEnv, cfg *config.Config) []checkResult {
	if e.timeBD == nil || len(cfg.Trackers) == 0 || sharedServer(cfg) {
		return nil
	}
	dir := cfg.Trackers[0].Path
	if _, err := os.Stat(filepath.Join(dir, ".beads")); err != nil {
		return nil
	}
	bd, err := e.lookPath("bd")
	if err != nil {
		return nil
	}
	r := checkResult{Name: "bd-latency"}
	probes := []struct {
		name string
		args []string
	}{
		{"list", []string{"list", "--limit", "1", "--json"}},
		{"metadata query", []string{"list", "--limit", "1", "--json", "--metadata-field", "lambe_kind=attempt"}},
	}
	var parts, slow []string
	for _, p := range probes {
		pctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		d, err := e.timeBD(pctx, bd, dir, p.args...)
		cancel()
		if err != nil {
			r.Status, r.Detail = statusWarn, fmt.Sprintf("bd %s failed: %v", p.name, err)
			return []checkResult{r}
		}
		parts = append(parts, fmt.Sprintf("%s %s", p.name, d.Round(10*time.Millisecond)))
		if d > slowBDThreshold {
			slow = append(slow, p.name)
		}
	}
	r.Detail = strings.Join(parts, ", ")
	if len(slow) == 0 {
		r.Status = statusOK
		return []checkResult{r}
	}
	r.Status = statusWarn
	r.Detail += fmt.Sprintf(" (over %s: %s)", slowBDThreshold, strings.Join(slow, ", "))
	r.Fix = "run Dolt in server mode (`bd init --server`), or raise supervisor.bd_timeout and lower supervisor.bd_max_concurrency; see docs/solutions/slow-embedded-dolt.md"
	return []checkResult{r}
}

func checkHarness(cfg *config.Config) checkResult {
	r := checkResult{Name: "harness"}
	if err := daemon.CheckHarness(cfg); err != nil {
		r.Status, r.Detail = statusFail, err.Error()
		r.Fix = "install the agent or change `harness` in the config; a service needs it on the unit's PATH too"
		return r
	}
	r.Status, r.Detail = statusOK, cmpOr(cfg.Harness, "pi")+" backend is runnable"
	return r
}

// lockHeld reports whether another process holds the daemon's state lock.
// It probes with a shared non-blocking flock and releases it immediately.
func lockHeld(path string) (held, exists bool) {
	f, err := os.Open(path) // #nosec G304 -- derived from the state directory
	if err != nil {
		return false, false
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK), true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false, true
}

// runFinished reports whether a live-snapshot run has reached a terminal phase
// ("stopped: gate green", "stopped: closed externally", ...). The daemon keeps
// finished runs in its snapshot, so they must not count as active or silent.
func runFinished(run daemon.LiveRun) bool {
	return strings.HasPrefix(run.Phase, "stopped")
}

// checkDaemon is the stall detector: is the supervisor alive, and is any
// run it owns past the silence timeout without output?
func checkDaemon(e doctorEnv, cfg *config.Config) []checkResult {
	lockFile := filepath.Join(e.stateDir, daemon.InstanceLockName(cfg))
	held, exists := lockHeld(lockFile)
	snap, alive, snapErr := daemon.ReadLive(daemon.LivePath(e.stateDir, cfg))
	hasSnap := snapErr == nil
	orphans := 0
	if hasSnap && !alive {
		for _, run := range snap.Runs {
			if run.WakeAt.IsZero() && !runFinished(run) {
				orphans++
			}
		}
	}

	d := checkResult{Name: "daemon"}
	switch {
	case held && alive:
		d.Status, d.Detail = statusOK, fmt.Sprintf("running (pid %d), state lock held in %s", snap.PID, e.stateDir)
	case held:
		d.Status = statusWarn
		d.Detail = "a daemon holds the state lock but publishes no live snapshot for this config"
		d.Fix = "it may be an older lambe or use a different LAMBE_STATE_DIR; restart it with the current binary"
	case orphans > 0:
		d.Status = statusFail
		d.Detail = fmt.Sprintf("daemon is not running but its last snapshot shows %d unfinished run(s): work is stalled", orphans)
		d.Fix = "start it again: `lambe service install` (supervised) or `lambe run`"
	default:
		d.Status = statusWarn
		if exists {
			d.Detail = "daemon is not running (lock file present, not held)"
		} else {
			d.Detail = "daemon has never run here (no state lock)"
		}
		d.Fix = "start it: `lambe service install` (supervised) or `lambe run`"
	}
	rs := []checkResult{d}
	if !held || !alive {
		return rs
	}

	silence := cfg.Supervisor.SilenceTimeout
	stall := cfg.Supervisor.StallAfter
	if stall <= 0 {
		stall = config.DefaultStallAfter
	}
	p := checkResult{Name: "progress", Status: statusOK, Detail: "no active runs"}
	var worst time.Duration
	var worstIssue string
	active := 0
	for _, run := range snap.Runs {
		if !run.WakeAt.IsZero() || runFinished(run) {
			continue // waiting on a scheduled retry, or already finished: not silent
		}
		active++
		last := run.UpdatedAt
		if run.LastOutputAt.After(last) {
			last = run.LastOutputAt
		}
		if run.LastActivityAt.After(last) {
			last = run.LastActivityAt
		}
		if age := e.now().Sub(last); age > worst {
			worst, worstIssue = age, run.IssueID
		}
	}
	if active > 0 {
		p.Detail = fmt.Sprintf("%d active run(s); quietest is %s, quiet for %s (watchdog reclaims at %s)", active, worstIssue, worst.Round(time.Second), silence)
		switch {
		case worst > silence:
			p.Status = statusFail
			p.Detail = fmt.Sprintf("%s has been silent for %s, past the %s silence timeout: the watchdog should have reclaimed it", worstIssue, worst.Round(time.Second), silence)
			p.Fix = "check `lambe logs -issue " + worstIssue + "` and the daemon log; restart the daemon if it is wedged"
		case worst > stall:
			p.Status = statusWarn
			p.Detail += fmt.Sprintf("; past the %s stall threshold", stall)
		}
	}
	rs = append(rs, p)
	return append(rs, checkLoop(e, cfg, snap))
}

// checkLoop reads the loop heartbeat the daemon stamps on every cycle. A
// live PID whose heartbeat stopped is a wedged tick loop: the process looks
// healthy while nothing is dispatched, heartbeated, or flushed.
func checkLoop(e doctorEnv, cfg *config.Config, snap daemon.LiveSnapshot) checkResult {
	r := checkResult{Name: "loop"}
	if snap.TickAt.IsZero() {
		r.Status, r.Detail = statusSkip, "daemon predates the loop heartbeat (no tick_at in its snapshot)"
		return r
	}
	tick := time.Duration(cfg.Supervisor.TickSeconds) * time.Second
	// One cycle can legitimately spend a bounded pull and a bounded flush
	// before it stamps again.
	bound := daemon.ControlTimeout(cfg)
	warnAt, failAt := bound+3*tick, 2*bound+3*tick
	age := e.now().Sub(snap.TickAt)
	r.Detail = fmt.Sprintf("last loop tick %s ago", age.Round(time.Second))
	switch {
	case age > failAt:
		r.Status = statusFail
		r.Detail += fmt.Sprintf(", past %s: the daemon is alive but its loop is wedged", failAt)
		r.Fix = "check the daemon log, then restart it (`lambe service install --force` or restart `lambe run`)"
	case age > warnAt:
		r.Status = statusWarn
		r.Detail += fmt.Sprintf(", past %s: a cycle is slow or hung", warnAt)
		r.Fix = "check the daemon log for hub sync or gate timeouts"
	default:
		r.Status = statusOK
	}
	return r
}

// checkService reports whether the OS keeps the daemon alive and whether
// the installed unit still matches this config.
func checkService(ctx context.Context, e doctorEnv, configPath string) checkResult {
	r := checkResult{Name: "service"}
	mgr, err := e.sys.managerFor("")
	if err != nil {
		r.Status, r.Detail = statusSkip, err.Error()
		return r
	}
	st := queryService(ctx, e.sys, mgr, "", true)
	switch {
	case !st.Installed:
		r.Status = statusWarn
		r.Detail = "not installed: if the daemon crashes or the machine reboots, nothing restarts it"
		r.Fix = "`lambe service install`"
	case !st.Running:
		r.Status, r.Detail = statusFail, st.Detail
		r.Fix = "`lambe service install --force` to reload it, then check " + e.sys.logPath()
	case !strings.Contains(st.Unit, configPath):
		r.Status = statusWarn
		r.Detail = st.Detail + ", but its unit points at a different config than " + configPath
		r.Fix = "`lambe service install --force`"
	default:
		r.Status, r.Detail = statusOK, st.Detail+" ("+st.Path+")"
	}
	return r
}

// checkStateAPI contacts the opt-in state API the same way the dashboard
// does: via the discovery descriptor, loopback only, no redirects.
func checkStateAPI(ctx context.Context, e doctorEnv, cfg *config.Config) checkResult {
	r := checkResult{Name: "stateapi"}
	if !cfg.StateAPI.Enable {
		r.Status, r.Detail = statusSkip, "disabled (optional; set stateapi.enable: true for the dashboard)"
		return r
	}
	data, err := os.ReadFile(daemon.StateAPIPath(e.stateDir, cfg)) // #nosec G304 -- derived from the state directory
	if err != nil {
		r.Status, r.Detail = statusWarn, "enabled but no descriptor found (daemon not running?)"
		r.Fix = "start the daemon, then re-run doctor"
		if ln, lerr := net.Listen("tcp", cfg.StateAPI.Addr); lerr != nil && errors.Is(lerr, syscall.EADDRINUSE) {
			// A fixed port that something else holds makes the daemon run without the API.
			r.Detail = "enabled but " + cfg.StateAPI.Addr + " is already in use, so the daemon runs without the state API"
			r.Fix = "stop the process holding the port or change stateapi.addr (port 0 picks a free one)"
		} else if lerr == nil {
			_ = ln.Close()
		}
		return r
	}
	var d daemon.StateAPIDescriptor
	if err := json.Unmarshal(data, &d); err != nil {
		r.Status, r.Detail = statusFail, "descriptor unreadable: "+err.Error()
		return r
	}
	host := d.Addr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	if host != "127.0.0.1" {
		r.Status, r.Detail = statusFail, "descriptor address is not loopback; refusing to send the token"
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	u := url.URL{Scheme: "http", Host: d.Addr, Path: "/api/v1/state"}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	req.Header.Set("Authorization", "Bearer "+d.BearerToken)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req) // #nosec G704 -- loopback address validated above
	if err != nil {
		r.Status, r.Detail = statusFail, fmt.Sprintf("descriptor says %s but nothing answers (stale descriptor from a crashed daemon?)", d.Addr)
		r.Fix = "restart the daemon"
		return r
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		r.Status, r.Detail = statusFail, fmt.Sprintf("GET /api/v1/state returned %s", resp.Status)
		return r
	}
	r.Status, r.Detail = statusOK, "answering on "+d.Addr
	return r
}

func checkRex(ctx context.Context, e doctorEnv) checkResult {
	av := rex.Detector{Env: e.getenv, Runner: e.rexRunner}.Detect(ctx)
	r := checkResult{Name: "rex"}
	switch {
	case av.Available:
		r.Status, r.Detail = statusOK, "Rex detected ("+av.Reason.String()+")"
	case av.Reason == rex.ReasonAbsent:
		r.Status, r.Detail = statusSkip, "Rex not running (optional)"
	default:
		r.Status, r.Detail = statusSkip, "Rex not detected (optional): "+av.Reason.String()
	}
	return r
}

// checkOrphanClaims warns about recorded claims that recovery cannot resume.
// It reads checkpoints only, so it costs no tracker call; it reports nothing
// when there are none.
func checkOrphanClaims(e doctorEnv, cfg *config.Config) []checkResult {
	claims, err := symphony.ScanOrphanClaims(daemon.CheckpointDir(e.stateDir, cfg))
	if err != nil || len(claims) == 0 {
		return nil
	}
	return []checkResult{{
		Name:   "claims",
		Status: statusWarn,
		Detail: fmt.Sprintf("%d orphaned claim(s) with no continuation state; the oldest is %s (%s)", len(claims), claims[0].Issue, e.now().Sub(claims[0].Since).Round(time.Minute)),
		Fix:    "review with `lambe claims list`; release each with `lambe claims release ISSUE` once its agent is confirmed gone",
	}}
}
