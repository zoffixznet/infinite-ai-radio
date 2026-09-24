// Package telemetry samples the machine resources phased generation
// spends: system memory, graphics memory, and which of the engine's
// models are sitting on the card right now. Phased generation exists to
// keep the card mostly empty, and without a window onto it the only
// evidence that it works is the absence of a crash. Sampling runs on
// its own timer and hands out the last snapshot, so a status display
// that repaints four times a second never pays for a probe.
package telemetry

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// interval is how often the sampler refreshes. Graphics memory moves in
// whole model loads, so a couple of seconds shows every transition
// while keeping nvidia-smi off the critical path.
const interval = 2 * time.Second

// probeTimeout bounds one nvidia-smi call; a wedged driver must not
// stall the sampler's goroutine forever.
const probeTimeout = 4 * time.Second

// Sample is one snapshot of the machine. Memory figures are bytes.
type Sample struct {
	// Taken is when the snapshot was measured; zero means the sampler
	// has not produced one yet.
	Taken time.Time

	// System memory, as the kernel reports it. RAMUsed is total minus
	// available, which counts reclaimable cache as free the way the
	// machine actually behaves.
	RAMTotal     uint64
	RAMAvailable uint64
	RAMUsed      uint64
	SwapTotal    uint64
	SwapUsed     uint64

	// RAMSelf is the resident memory of everything working for the
	// radio: this process, the engine daemon and everything it spawned,
	// and - while the lyric writer is writing for the radio - the
	// writer's processes too. The RAM row colors this share separately,
	// so "how much of that is us" has an answer at a glance. CPUSelf is
	// the same idea for the processor: those processes' share of the
	// machine over the last sampling interval, 0-100; -1 until two
	// samples exist.
	RAMSelf uint64
	CPUSelf int

	// CPUUtil is whole-machine processor use over the last sampling
	// interval, 0-100; -1 until two samples exist to compare. Load1 is
	// the one-minute load average.
	CPUUtil int
	Load1   float64

	// GPUPresent reports whether a graphics card was found. GPUError
	// carries why not, when something went wrong rather than there
	// simply being no card.
	GPUPresent bool
	GPUError   string
	GPUName    string
	VRAMTotal  uint64
	VRAMUsed   uint64
	VRAMFree   uint64
	GPUUtil    int // percent
	GPUTemp    int // degrees C

	// Procs lists every process holding graphics memory, largest first.
	Procs []GPUProc

	// Models lists the engine models currently resident on the card,
	// newest load last. Empty while the engine is stopped.
	Models []Model

	// EnginePID is the engine daemon this radio owns (0 while it is
	// stopped), and EngineVRAM is what it and its children hold.
	EnginePID  int
	EngineVRAM uint64

	// WriterBusy reports the lyric writer is writing the radio's
	// songs: a wordsmith round is under way, or one ended moments ago.
	// The writer is a service of its own rather than a child of
	// anything here, so it is found by name; while it writes for the
	// radio its processes count as the radio's, and while it answers
	// anything else - the radio's own health check and steers included
	// - they do not, and the shared row names it. WriterName is
	// the process holding its card memory (or its runner, or the daemon
	// itself, when nothing of it is on the card; empty when none was
	// found on this machine), WriterVRAM what those processes hold on
	// the card and WriterRAM their resident memory - both already
	// folded into the radio's figures, and zero while the writer is
	// not working.
	WriterBusy bool
	WriterName string
	WriterVRAM uint64
	WriterRAM  uint64
}

// RadioVRAM is the graphics memory held for the radio right now: the
// engine's, plus the writer's while it writes for the radio.
func (s Sample) RadioVRAM() uint64 { return s.EngineVRAM + s.WriterVRAM }

// GPUProc is one process holding graphics memory.
type GPUProc struct {
	PID  int
	Name string
	VRAM uint64
	// Engine marks this radio's own engine process, and Writer the
	// lyric writer's while it works for the radio, as opposed to
	// whatever else shares the card.
	Engine bool
	Writer bool
}

// Radio reports whether the process is working for the radio.
func (p GPUProc) Radio() bool { return p.Engine || p.Writer }

// Model is one engine model resident on the card.
type Model struct {
	// Name is the model's role in plain words.
	Name string
	// Bytes is how much it occupies, or 0 when the engine did not say.
	Bytes uint64
	// Since is when it was loaded.
	Since time.Time
	// FromDisk marks weights streamed from disk rather than held in
	// system memory between uses.
	FromDisk bool
}

// Sampler measures on a timer and hands out the last measurement.
type Sampler struct {
	enginePID  func() int
	writerBusy func() bool
	models     *modelTracker

	mu  sync.Mutex
	cur Sample
	// prevIdle/prevTotal are the last /proc/stat readings, so CPU use
	// can be computed as a delta between samples; prevSelfJiffies is
	// the radio tree's own accumulated CPU time at the last sample,
	// and prevWriterJiffies the writer's - kept apart, so the writer
	// joining the radio's figure adds the work it did since the last
	// sample rather than everything it has ever done.
	prevIdle, prevTotal uint64
	prevSelfJiffies     uint64
	prevWriterJiffies   uint64
	lastTotalDelta      int64
}

// New returns an unstarted sampler. daemonLog is the engine daemon's
// output file, read for model load and offload events; enginePID
// reports the daemon's process id, or 0 while it is stopped (nil is
// allowed, and gives up on attributing graphics memory to this radio);
// writerBusy reports whether the lyric writer is writing the radio's
// songs right now (nil never counts it).
func New(daemonLog string, enginePID func() int, writerBusy func() bool) *Sampler {
	if enginePID == nil {
		enginePID = func() int { return 0 }
	}
	if writerBusy == nil {
		writerBusy = func() bool { return false }
	}
	return &Sampler{enginePID: enginePID, writerBusy: writerBusy, models: newModelTracker(daemonLog)}
}

// Start begins sampling until ctx ends. The first sample is taken
// synchronously so an immediate display has something to show.
func (s *Sampler) Start(ctx context.Context) {
	s.refresh()
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.refresh()
			}
		}
	}()
}

// Sample returns the most recent snapshot. It never blocks on a probe,
// so status displays can call it as often as they repaint.
func (s *Sampler) Sample() Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// refresh takes one measurement and publishes it.
func (s *Sampler) refresh() {
	var next Sample
	next.Taken = time.Now()
	readMeminfo(&next)
	s.readCPU(&next)
	// One pass over /proc serves every question about processes this
	// sample asks: the engine's tree, the writer's, and whose the
	// card's holders are.
	table := readProcTable()
	pid := s.enginePID()
	next.EnginePID = pid
	radio := []int{os.Getpid()}
	if pid > 0 {
		radio = append(radio, table.descendants(pid)...)
	}
	writer := table.writerTree()
	next.WriterBusy = s.writerBusy()
	next.RAMSelf, next.CPUSelf = s.selfTree(radio, writer, &next)
	readGPU(&next, pid, table, writer)
	if next.WriterBusy && next.WriterName == "" {
		// Nothing of the writer on the card (it is working from system
		// memory): name it by its runner, or by the daemon itself.
		next.WriterName = table.writerName(writer)
	}
	// The daemon's own log says which models it moved onto the card,
	// but a daemon that has died since takes everything with it - so
	// the log is only trusted while the process still holds memory.
	if pid > 0 && next.EngineVRAM > 0 {
		next.Models = s.models.resident()
	} else {
		s.models.reset()
	}
	s.mu.Lock()
	s.cur = next
	s.mu.Unlock()
}

// readCPU fills in whole-machine processor use from /proc/stat as a
// delta against the previous sample, and the one-minute load average
// from /proc/loadavg.
func (s *Sampler) readCPU(out *Sample) {
	out.CPUUtil = -1
	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(raw)); len(f) > 0 {
			out.Load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return
	}
	line, _, _ := strings.Cut(string(raw), "\n")
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return
	}
	// Only the first eight value columns (user..steal) are summed:
	// guest and guest_nice are already counted inside user and nice,
	// so including them would dilute the busy fraction on a machine
	// running virtual machines.
	vals := fields[1:]
	if len(vals) > 8 {
		vals = vals[:8]
	}
	var idle, total uint64
	for i, f := range vals {
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return
		}
		total += v
		// idle is the 4th column, iowait the 5th; both count as the
		// machine not working.
		if i == 3 || i == 4 {
			idle += v
		}
	}
	prevIdle, prevTotal := s.prevIdle, s.prevTotal
	s.prevIdle, s.prevTotal = idle, total
	if prevTotal == 0 || total <= prevTotal {
		return
	}
	// Signed math: iowait can tick backwards, and an unsigned wrap
	// would read as astronomically busy.
	totalDelta := int64(total) - int64(prevTotal)
	idleDelta := int64(idle) - int64(prevIdle)
	s.lastTotalDelta = totalDelta
	busy := float64(totalDelta-idleDelta) / float64(totalDelta)
	if busy < 0 {
		busy = 0
	}
	if busy > 1 {
		busy = 1
	}
	out.CPUUtil = int(busy*100 + 0.5)
}

// selfTree sums the resident memory and CPU share of the radio's
// processes - this one plus, when the engine daemon is running, its
// whole tree, the daemon and the Python engine it launches - and, while
// the writer works for the radio, the writer's tree too. The CPU share
// is the jiffies delta against the machine's, using the totals readCPU
// measured for this sample. The writer's own counters are read every
// sample whether or not it is folded in, so the moment it joins the
// figure its delta is the work of one interval, not of its lifetime.
func (s *Sampler) selfTree(radio, writer []int, out *Sample) (rss uint64, cpu int) {
	rss, jiffies := treeStats(radio)
	writerRSS, writerJiffies := treeStats(writer)
	prev, prevWriter := s.prevSelfJiffies, s.prevWriterJiffies
	s.prevSelfJiffies, s.prevWriterJiffies = jiffies, writerJiffies
	if out.WriterBusy {
		out.WriterRAM = writerRSS
		rss += writerRSS
	}
	cpu = -1
	// A shrinking tree (the engine died) drops the sum below the
	// previous reading; that sample simply has no self figure.
	if prev > 0 && jiffies >= prev && out.CPUUtil >= 0 && s.lastTotalDelta > 0 {
		delta := jiffies - prev
		if out.WriterBusy && prevWriter > 0 && writerJiffies >= prevWriter {
			delta += writerJiffies - prevWriter
		}
		cpu = cpuShare(delta, s.lastTotalDelta)
	}
	return rss, cpu
}

// cpuShare turns a tree's jiffies over an interval into its percent
// share of the machine's, clamped to 0-100.
func cpuShare(delta uint64, totalDelta int64) int {
	if totalDelta <= 0 {
		return -1
	}
	frac := float64(delta) / float64(totalDelta)
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	return int(frac*100 + 0.5)
}

// treeStats sums resident memory and accumulated CPU time over a set
// of processes.
func treeStats(pids []int) (rss, jiffies uint64) {
	for _, pid := range pids {
		r, j := statsOf(pid)
		rss += r
		jiffies += j
	}
	return rss, jiffies
}

// procTable is one snapshot of the process table - every process's
// parent, children and short name - from a single pass over /proc.
type procTable struct {
	parent   map[int]int
	children map[int][]int
	comm     map[int]string
}

// newProcTable builds a table from parent links and names; the
// children index follows from them.
func newProcTable(parent map[int]int, comm map[int]string) procTable {
	t := procTable{parent: parent, children: map[int][]int{}, comm: comm}
	for p, pp := range parent {
		t.children[pp] = append(t.children[pp], p)
	}
	return t
}

// readProcTable takes the snapshot.
func readProcTable() procTable {
	parent, comm := map[int]int{}, map[int]string{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return newProcTable(parent, comm)
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if c, ppid, ok := readStat(pid); ok {
			parent[pid] = ppid
			comm[pid] = c
		}
	}
	return newProcTable(parent, comm)
}

// descendants returns pid and every transitive child, parents before
// children.
func (t procTable) descendants(pid int) []int {
	out := []int{pid}
	for i := 0; i < len(out); i++ {
		out = append(out, t.children[out[i]]...)
	}
	return out
}

// writerTree finds the lyric writer on this machine: every process
// named for the writer's daemon, with everything it spawned. Empty
// when the writer runs elsewhere, or not at all.
func (t procTable) writerTree() []int {
	var out []int
	seen := map[int]bool{}
	for pid, comm := range t.comm {
		if comm != writerDaemon {
			continue
		}
		for _, p := range t.descendants(pid) {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// writerName names a writer tree for the readout: the runner it has
// spawned, or the daemon itself when it has none. Empty for an empty
// tree.
func (t procTable) writerName(writer []int) string {
	name := ""
	for _, p := range writer {
		comm := t.comm[p]
		if comm == "" {
			continue
		}
		if comm != writerDaemon {
			return comm
		}
		name = comm
	}
	return name
}

// writerDaemon is the short name of the lyric writer's daemon.
const writerDaemon = "ollama"

// writerRunner recognises the process the writer's daemon runs a model
// in: llama-server in current releases, a second "ollama" (the runner
// subcommand) in older ones. The daemon's own name is truncated to
// fifteen characters by the kernel, which is why prefixes are matched.
func writerRunner(name string) bool {
	return strings.HasPrefix(name, "llama-server") || strings.HasPrefix(name, "ollama")
}

// statsOf reads one process's resident memory in bytes and its
// accumulated CPU time in jiffies (user plus system).
func statsOf(pid int) (rss, jiffies uint64) {
	dir := "/proc/" + strconv.Itoa(pid)
	if raw, err := os.ReadFile(dir + "/statm"); err == nil {
		if f := strings.Fields(string(raw)); len(f) >= 2 {
			if pages, err := strconv.ParseUint(f[1], 10, 64); err == nil {
				rss = pages * uint64(os.Getpagesize())
			}
		}
	}
	raw, err := os.ReadFile(dir + "/stat")
	if err != nil {
		return rss, 0
	}
	i := strings.LastIndexByte(string(raw), ')')
	if i < 0 {
		return rss, 0
	}
	f := strings.Fields(string(raw[i+1:]))
	// After the comm field: state is index 0, utime 11, stime 12.
	if len(f) > 12 {
		u, _ := strconv.ParseUint(f[11], 10, 64)
		sys, _ := strconv.ParseUint(f[12], 10, 64)
		jiffies = u + sys
	}
	return rss, jiffies
}

// readMeminfo fills in the system memory figures from /proc/meminfo.
func readMeminfo(out *Sample) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return
	}
	defer f.Close()
	var swapFree uint64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		// Every value of interest is reported in kB.
		fields := strings.Fields(val)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		n *= 1024
		switch key {
		case "MemTotal":
			out.RAMTotal = n
		case "MemAvailable":
			out.RAMAvailable = n
		case "SwapTotal":
			out.SwapTotal = n
		case "SwapFree":
			swapFree = n
		}
	}
	if out.RAMTotal > out.RAMAvailable {
		out.RAMUsed = out.RAMTotal - out.RAMAvailable
	}
	if out.SwapTotal > swapFree {
		out.SwapUsed = out.SwapTotal - swapFree
	}
}

// readGPU fills in the graphics figures via nvidia-smi, attributing
// what it can to the engine daemon named by pid and, while the writer
// is working for the radio, to the writer's processes.
func readGPU(out *Sample, pid int, table procTable, writer []int) {
	line, err := nvidiaSMI("--query-gpu=name,memory.total,memory.used,memory.free,utilization.gpu,temperature.gpu")
	if err != nil {
		// No card, or no driver: not an error worth shouting about,
		// but worth showing instead of a row of zeroes.
		out.GPUError = err.Error()
		return
	}
	rows := splitRows(line)
	if len(rows) == 0 {
		out.GPUError = "nvidia-smi reported no devices"
		return
	}
	// One card is the supported shape; the first is the one the engine
	// uses.
	f := rows[0]
	if len(f) < 6 {
		out.GPUError = "unexpected nvidia-smi output"
		return
	}
	out.GPUPresent = true
	out.GPUName = f[0]
	out.VRAMTotal = mib(f[1])
	out.VRAMUsed = mib(f[2])
	out.VRAMFree = mib(f[3])
	out.GPUUtil = atoi(f[4])
	out.GPUTemp = atoi(f[5])

	apps, err := nvidiaSMI("--query-compute-apps=pid,used_gpu_memory")
	if err != nil {
		return
	}
	var held []GPUProc
	for _, r := range splitRows(apps) {
		if len(r) < 2 {
			continue
		}
		p := atoi(r[0])
		if p == 0 {
			continue
		}
		held = append(held, GPUProc{PID: p, Name: processName(p), VRAM: mib(r[1])})
	}
	attribute(out, held, table.parent, pid, writer)
}

// attribute decides whose each card-holding process is and sums the
// radio's share. The engine can only be recognised through ancestry:
// the daemon is a Go process and the memory is held by the Python
// server it supervises. The writer's processes are the ones found by
// name, and any holder named like the writer's runner - and they count
// as the radio's only while the writer is working for it.
func attribute(out *Sample, held []GPUProc, parents map[int]int, enginePID int, writer []int) {
	isWriter := map[int]bool{}
	for _, p := range writer {
		isWriter[p] = true
	}
	// The writer is named after whichever of its processes holds the
	// most card memory.
	var named uint64
	for _, proc := range held {
		proc.Engine = enginePID > 0 && descendsFrom(parents, proc.PID, enginePID)
		if proc.Engine {
			out.EngineVRAM += proc.VRAM
		} else if out.WriterBusy && (isWriter[proc.PID] || writerRunner(proc.Name)) {
			proc.Writer = true
			out.WriterVRAM += proc.VRAM
			if proc.VRAM > named {
				named = proc.VRAM
				out.WriterName = proc.Name
			}
		}
		out.Procs = append(out.Procs, proc)
	}
	// Largest first: the interesting question is always who is using
	// the card, not who happens to have the lowest pid.
	for i := 1; i < len(out.Procs); i++ {
		for j := i; j > 0 && out.Procs[j].VRAM > out.Procs[j-1].VRAM; j-- {
			out.Procs[j], out.Procs[j-1] = out.Procs[j-1], out.Procs[j]
		}
	}
}

// nvidiaSMI runs one query and returns its raw output.
func nvidiaSMI(query string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nvidia-smi", query, "--format=csv,noheader,nounits")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// splitRows turns nvidia-smi's headerless CSV into trimmed fields.
func splitRows(s string) [][]string {
	var rows [][]string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		rows = append(rows, parts)
	}
	return rows
}

// mib parses a mebibyte count into bytes; nounits output is bare
// numbers, but a stray "MiB" suffix costs nothing to tolerate.
func mib(s string) uint64 {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "MiB"))
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n * 1024 * 1024
}

func atoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

// processName reads a process's short name, falling back to its pid.
func processName(pid int) string {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	if err == nil {
		if name := strings.TrimSpace(string(raw)); name != "" {
			// A bare "python" says nothing; the script it runs does.
			if name == "python" || name == "python3" {
				if arg := scriptArg(pid); arg != "" {
					return name + " (" + arg + ")"
				}
			}
			return name
		}
	}
	return "pid " + strconv.Itoa(pid)
}

// scriptArg picks the most identifying word out of a Python command
// line: the module or script it was pointed at.
func scriptArg(pid int) string {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return ""
	}
	args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a == "-m" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		if idx := strings.LastIndex(a, "/"); idx >= 0 {
			a = a[idx+1:]
		}
		if a != "" {
			return a
		}
	}
	return ""
}

// readStat reads one process's short name and parent from
// /proc/<pid>/stat. The comm field can contain spaces and parentheses,
// so it is cut between the first '(' and the last ')', and the fields
// after that are the ones that can be split safely.
func readStat(pid int) (comm string, ppid int, ok bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", 0, false
	}
	s := string(raw)
	open := strings.IndexByte(s, '(')
	idx := strings.LastIndexByte(s, ')')
	if open < 0 || idx < open {
		return "", 0, false
	}
	comm = s[open+1 : idx]
	fields := strings.Fields(s[idx+1:])
	// fields[0] is state, fields[1] is ppid.
	if len(fields) < 2 {
		return "", 0, false
	}
	ppid, err = strconv.Atoi(fields[1])
	if err != nil {
		return "", 0, false
	}
	return comm, ppid, true
}

// descendsFrom reports whether pid is want, or any descendant of it.
func descendsFrom(parents map[int]int, pid, want int) bool {
	// Bounded so a cycle in a torn read of /proc cannot spin forever.
	for i := 0; pid > 1 && i < 64; i++ {
		if pid == want {
			return true
		}
		next, ok := parents[pid]
		if !ok {
			return false
		}
		pid = next
	}
	return pid == want
}
