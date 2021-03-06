// Copyright 2019 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package proc

import (
	"bytes"
	"fmt"
	"io"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/kernfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/limits"
	"gvisor.dev/gvisor/pkg/sentry/mm"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/syserror"
	"gvisor.dev/gvisor/pkg/usermem"
)

// mm gets the kernel task's MemoryManager. No additional reference is taken on
// mm here. This is safe because MemoryManager.destroy is required to leave the
// MemoryManager in a state where it's still usable as a DynamicBytesSource.
func getMM(task *kernel.Task) *mm.MemoryManager {
	var tmm *mm.MemoryManager
	task.WithMuLocked(func(t *kernel.Task) {
		if mm := t.MemoryManager(); mm != nil {
			tmm = mm
		}
	})
	return tmm
}

// getMMIncRef returns t's MemoryManager. If getMMIncRef succeeds, the
// MemoryManager's users count is incremented, and must be decremented by the
// caller when it is no longer in use.
func getMMIncRef(task *kernel.Task) (*mm.MemoryManager, error) {
	if task.ExitState() == kernel.TaskExitDead {
		return nil, syserror.ESRCH
	}
	var m *mm.MemoryManager
	task.WithMuLocked(func(t *kernel.Task) {
		m = t.MemoryManager()
	})
	if m == nil || !m.IncUsers() {
		return nil, io.EOF
	}
	return m, nil
}

type bufferWriter struct {
	buf *bytes.Buffer
}

// WriteFromBlocks writes up to srcs.NumBytes() bytes from srcs and returns
// the number of bytes written. It may return a partial write without an
// error (i.e. (n, nil) where 0 < n < srcs.NumBytes()). It should not
// return a full write with an error (i.e. srcs.NumBytes(), err) where err
// != nil).
func (w *bufferWriter) WriteFromBlocks(srcs safemem.BlockSeq) (uint64, error) {
	written := srcs.NumBytes()
	for !srcs.IsEmpty() {
		w.buf.Write(srcs.Head().ToSlice())
		srcs = srcs.Tail()
	}
	return written, nil
}

// auxvData implements vfs.DynamicBytesSource for /proc/[pid]/auxv.
//
// +stateify savable
type auxvData struct {
	kernfs.DynamicBytesFile

	task *kernel.Task
}

var _ dynamicInode = (*auxvData)(nil)

// Generate implements vfs.DynamicBytesSource.Generate.
func (d *auxvData) Generate(ctx context.Context, buf *bytes.Buffer) error {
	m, err := getMMIncRef(d.task)
	if err != nil {
		return err
	}
	defer m.DecUsers(ctx)

	// Space for buffer with AT_NULL (0) terminator at the end.
	auxv := m.Auxv()
	buf.Grow((len(auxv) + 1) * 16)
	for _, e := range auxv {
		var tmp [8]byte
		usermem.ByteOrder.PutUint64(tmp[:], e.Key)
		buf.Write(tmp[:])

		usermem.ByteOrder.PutUint64(tmp[:], uint64(e.Value))
		buf.Write(tmp[:])
	}
	return nil
}

// execArgType enumerates the types of exec arguments that are exposed through
// proc.
type execArgType int

const (
	cmdlineDataArg execArgType = iota
	environDataArg
)

// cmdlineData implements vfs.DynamicBytesSource for /proc/[pid]/cmdline.
//
// +stateify savable
type cmdlineData struct {
	kernfs.DynamicBytesFile

	task *kernel.Task

	// arg is the type of exec argument this file contains.
	arg execArgType
}

var _ dynamicInode = (*cmdlineData)(nil)

// Generate implements vfs.DynamicBytesSource.Generate.
func (d *cmdlineData) Generate(ctx context.Context, buf *bytes.Buffer) error {
	m, err := getMMIncRef(d.task)
	if err != nil {
		return err
	}
	defer m.DecUsers(ctx)

	// Figure out the bounds of the exec arg we are trying to read.
	var ar usermem.AddrRange
	switch d.arg {
	case cmdlineDataArg:
		ar = usermem.AddrRange{
			Start: m.ArgvStart(),
			End:   m.ArgvEnd(),
		}
	case environDataArg:
		ar = usermem.AddrRange{
			Start: m.EnvvStart(),
			End:   m.EnvvEnd(),
		}
	default:
		panic(fmt.Sprintf("unknown exec arg type %v", d.arg))
	}
	if ar.Start == 0 || ar.End == 0 {
		// Don't attempt to read before the start/end are set up.
		return io.EOF
	}

	// N.B. Technically this should be usermem.IOOpts.IgnorePermissions = true
	// until Linux 4.9 (272ddc8b3735 "proc: don't use FOLL_FORCE for reading
	// cmdline and environment").
	writer := &bufferWriter{buf: buf}
	if n, err := m.CopyInTo(ctx, usermem.AddrRangeSeqOf(ar), writer, usermem.IOOpts{}); n == 0 || err != nil {
		// Nothing to copy or something went wrong.
		return err
	}

	// On Linux, if the NULL byte at the end of the argument vector has been
	// overwritten, it continues reading the environment vector as part of
	// the argument vector.
	if d.arg == cmdlineDataArg && buf.Bytes()[buf.Len()-1] != 0 {
		if end := bytes.IndexByte(buf.Bytes(), 0); end != -1 {
			// If we found a NULL character somewhere else in argv, truncate the
			// return up to the NULL terminator (including it).
			buf.Truncate(end)
			return nil
		}

		// There is no NULL terminator in the string, return into envp.
		arEnvv := usermem.AddrRange{
			Start: m.EnvvStart(),
			End:   m.EnvvEnd(),
		}

		// Upstream limits the returned amount to one page of slop.
		// https://elixir.bootlin.com/linux/v4.20/source/fs/proc/base.c#L208
		// we'll return one page total between argv and envp because of the
		// above page restrictions.
		if buf.Len() >= usermem.PageSize {
			// Returned at least one page already, nothing else to add.
			return nil
		}
		remaining := usermem.PageSize - buf.Len()
		if int(arEnvv.Length()) > remaining {
			end, ok := arEnvv.Start.AddLength(uint64(remaining))
			if !ok {
				return syserror.EFAULT
			}
			arEnvv.End = end
		}
		if _, err := m.CopyInTo(ctx, usermem.AddrRangeSeqOf(arEnvv), writer, usermem.IOOpts{}); err != nil {
			return err
		}

		// Linux will return envp up to and including the first NULL character,
		// so find it.
		if end := bytes.IndexByte(buf.Bytes()[ar.Length():], 0); end != -1 {
			buf.Truncate(end)
		}
	}

	return nil
}

// +stateify savable
type commInode struct {
	kernfs.DynamicBytesFile

	task *kernel.Task
}

func newComm(task *kernel.Task, ino uint64, perm linux.FileMode) *kernfs.Dentry {
	inode := &commInode{task: task}
	inode.DynamicBytesFile.Init(task.Credentials(), ino, &commData{task: task}, perm)

	d := &kernfs.Dentry{}
	d.Init(inode)
	return d
}

func (i *commInode) CheckPermissions(ctx context.Context, creds *auth.Credentials, ats vfs.AccessTypes) error {
	// This file can always be read or written by members of the same thread
	// group. See fs/proc/base.c:proc_tid_comm_permission.
	//
	// N.B. This check is currently a no-op as we don't yet support writing and
	// this file is world-readable anyways.
	t := kernel.TaskFromContext(ctx)
	if t != nil && t.ThreadGroup() == i.task.ThreadGroup() && !ats.MayExec() {
		return nil
	}

	return i.DynamicBytesFile.CheckPermissions(ctx, creds, ats)
}

// commData implements vfs.DynamicBytesSource for /proc/[pid]/comm.
//
// +stateify savable
type commData struct {
	kernfs.DynamicBytesFile

	task *kernel.Task
}

var _ dynamicInode = (*commData)(nil)

// Generate implements vfs.DynamicBytesSource.Generate.
func (d *commData) Generate(ctx context.Context, buf *bytes.Buffer) error {
	buf.WriteString(d.task.Name())
	buf.WriteString("\n")
	return nil
}

// idMapData implements vfs.DynamicBytesSource for /proc/[pid]/{gid_map|uid_map}.
//
// +stateify savable
type idMapData struct {
	kernfs.DynamicBytesFile

	task *kernel.Task
	gids bool
}

var _ dynamicInode = (*idMapData)(nil)

// Generate implements vfs.DynamicBytesSource.Generate.
func (d *idMapData) Generate(ctx context.Context, buf *bytes.Buffer) error {
	var entries []auth.IDMapEntry
	if d.gids {
		entries = d.task.UserNamespace().GIDMap()
	} else {
		entries = d.task.UserNamespace().UIDMap()
	}
	for _, e := range entries {
		fmt.Fprintf(buf, "%10d %10d %10d\n", e.FirstID, e.FirstParentID, e.Length)
	}
	return nil
}

// mapsData implements vfs.DynamicBytesSource for /proc/[pid]/maps.
//
// +stateify savable
type mapsData struct {
	kernfs.DynamicBytesFile

	task *kernel.Task
}

var _ dynamicInode = (*mapsData)(nil)

// Generate implements vfs.DynamicBytesSource.Generate.
func (d *mapsData) Generate(ctx context.Context, buf *bytes.Buffer) error {
	if mm := getMM(d.task); mm != nil {
		mm.ReadMapsDataInto(ctx, buf)
	}
	return nil
}

// smapsData implements vfs.DynamicBytesSource for /proc/[pid]/smaps.
//
// +stateify savable
type smapsData struct {
	kernfs.DynamicBytesFile

	task *kernel.Task
}

var _ dynamicInode = (*smapsData)(nil)

// Generate implements vfs.DynamicBytesSource.Generate.
func (d *smapsData) Generate(ctx context.Context, buf *bytes.Buffer) error {
	if mm := getMM(d.task); mm != nil {
		mm.ReadSmapsDataInto(ctx, buf)
	}
	return nil
}

// +stateify savable
type taskStatData struct {
	kernfs.DynamicBytesFile

	task *kernel.Task

	// If tgstats is true, accumulate fault stats (not implemented) and CPU
	// time across all tasks in t's thread group.
	tgstats bool

	// pidns is the PID namespace associated with the proc filesystem that
	// includes the file using this statData.
	pidns *kernel.PIDNamespace
}

var _ dynamicInode = (*taskStatData)(nil)

// Generate implements vfs.DynamicBytesSource.Generate.
func (s *taskStatData) Generate(ctx context.Context, buf *bytes.Buffer) error {
	fmt.Fprintf(buf, "%d ", s.pidns.IDOfTask(s.task))
	fmt.Fprintf(buf, "(%s) ", s.task.Name())
	fmt.Fprintf(buf, "%c ", s.task.StateStatus()[0])
	ppid := kernel.ThreadID(0)
	if parent := s.task.Parent(); parent != nil {
		ppid = s.pidns.IDOfThreadGroup(parent.ThreadGroup())
	}
	fmt.Fprintf(buf, "%d ", ppid)
	fmt.Fprintf(buf, "%d ", s.pidns.IDOfProcessGroup(s.task.ThreadGroup().ProcessGroup()))
	fmt.Fprintf(buf, "%d ", s.pidns.IDOfSession(s.task.ThreadGroup().Session()))
	fmt.Fprintf(buf, "0 0 " /* tty_nr tpgid */)
	fmt.Fprintf(buf, "0 " /* flags */)
	fmt.Fprintf(buf, "0 0 0 0 " /* minflt cminflt majflt cmajflt */)
	var cputime usage.CPUStats
	if s.tgstats {
		cputime = s.task.ThreadGroup().CPUStats()
	} else {
		cputime = s.task.CPUStats()
	}
	fmt.Fprintf(buf, "%d %d ", linux.ClockTFromDuration(cputime.UserTime), linux.ClockTFromDuration(cputime.SysTime))
	cputime = s.task.ThreadGroup().JoinedChildCPUStats()
	fmt.Fprintf(buf, "%d %d ", linux.ClockTFromDuration(cputime.UserTime), linux.ClockTFromDuration(cputime.SysTime))
	fmt.Fprintf(buf, "%d %d ", s.task.Priority(), s.task.Niceness())
	fmt.Fprintf(buf, "%d ", s.task.ThreadGroup().Count())

	// itrealvalue. Since kernel 2.6.17, this field is no longer
	// maintained, and is hard coded as 0.
	fmt.Fprintf(buf, "0 ")

	// Start time is relative to boot time, expressed in clock ticks.
	fmt.Fprintf(buf, "%d ", linux.ClockTFromDuration(s.task.StartTime().Sub(s.task.Kernel().Timekeeper().BootTime())))

	var vss, rss uint64
	s.task.WithMuLocked(func(t *kernel.Task) {
		if mm := t.MemoryManager(); mm != nil {
			vss = mm.VirtualMemorySize()
			rss = mm.ResidentSetSize()
		}
	})
	fmt.Fprintf(buf, "%d %d ", vss, rss/usermem.PageSize)

	// rsslim.
	fmt.Fprintf(buf, "%d ", s.task.ThreadGroup().Limits().Get(limits.Rss).Cur)

	fmt.Fprintf(buf, "0 0 0 0 0 " /* startcode endcode startstack kstkesp kstkeip */)
	fmt.Fprintf(buf, "0 0 0 0 0 " /* signal blocked sigignore sigcatch wchan */)
	fmt.Fprintf(buf, "0 0 " /* nswap cnswap */)
	terminationSignal := linux.Signal(0)
	if s.task == s.task.ThreadGroup().Leader() {
		terminationSignal = s.task.ThreadGroup().TerminationSignal()
	}
	fmt.Fprintf(buf, "%d ", terminationSignal)
	fmt.Fprintf(buf, "0 0 0 " /* processor rt_priority policy */)
	fmt.Fprintf(buf, "0 0 0 " /* delayacct_blkio_ticks guest_time cguest_time */)
	fmt.Fprintf(buf, "0 0 0 0 0 0 0 " /* start_data end_data start_brk arg_start arg_end env_start env_end */)
	fmt.Fprintf(buf, "0\n" /* exit_code */)

	return nil
}

// statmData implements vfs.DynamicBytesSource for /proc/[pid]/statm.
//
// +stateify savable
type statmData struct {
	kernfs.DynamicBytesFile

	task *kernel.Task
}

var _ dynamicInode = (*statmData)(nil)

// Generate implements vfs.DynamicBytesSource.Generate.
func (s *statmData) Generate(ctx context.Context, buf *bytes.Buffer) error {
	var vss, rss uint64
	s.task.WithMuLocked(func(t *kernel.Task) {
		if mm := t.MemoryManager(); mm != nil {
			vss = mm.VirtualMemorySize()
			rss = mm.ResidentSetSize()
		}
	})

	fmt.Fprintf(buf, "%d %d 0 0 0 0 0\n", vss/usermem.PageSize, rss/usermem.PageSize)
	return nil
}

// statusData implements vfs.DynamicBytesSource for /proc/[pid]/status.
//
// +stateify savable
type statusData struct {
	kernfs.DynamicBytesFile

	task  *kernel.Task
	pidns *kernel.PIDNamespace
}

var _ dynamicInode = (*statusData)(nil)

// Generate implements vfs.DynamicBytesSource.Generate.
func (s *statusData) Generate(ctx context.Context, buf *bytes.Buffer) error {
	fmt.Fprintf(buf, "Name:\t%s\n", s.task.Name())
	fmt.Fprintf(buf, "State:\t%s\n", s.task.StateStatus())
	fmt.Fprintf(buf, "Tgid:\t%d\n", s.pidns.IDOfThreadGroup(s.task.ThreadGroup()))
	fmt.Fprintf(buf, "Pid:\t%d\n", s.pidns.IDOfTask(s.task))
	ppid := kernel.ThreadID(0)
	if parent := s.task.Parent(); parent != nil {
		ppid = s.pidns.IDOfThreadGroup(parent.ThreadGroup())
	}
	fmt.Fprintf(buf, "PPid:\t%d\n", ppid)
	tpid := kernel.ThreadID(0)
	if tracer := s.task.Tracer(); tracer != nil {
		tpid = s.pidns.IDOfTask(tracer)
	}
	fmt.Fprintf(buf, "TracerPid:\t%d\n", tpid)
	var fds int
	var vss, rss, data uint64
	s.task.WithMuLocked(func(t *kernel.Task) {
		if fdTable := t.FDTable(); fdTable != nil {
			fds = fdTable.Size()
		}
		if mm := t.MemoryManager(); mm != nil {
			vss = mm.VirtualMemorySize()
			rss = mm.ResidentSetSize()
			data = mm.VirtualDataSize()
		}
	})
	fmt.Fprintf(buf, "FDSize:\t%d\n", fds)
	fmt.Fprintf(buf, "VmSize:\t%d kB\n", vss>>10)
	fmt.Fprintf(buf, "VmRSS:\t%d kB\n", rss>>10)
	fmt.Fprintf(buf, "VmData:\t%d kB\n", data>>10)
	fmt.Fprintf(buf, "Threads:\t%d\n", s.task.ThreadGroup().Count())
	creds := s.task.Credentials()
	fmt.Fprintf(buf, "CapInh:\t%016x\n", creds.InheritableCaps)
	fmt.Fprintf(buf, "CapPrm:\t%016x\n", creds.PermittedCaps)
	fmt.Fprintf(buf, "CapEff:\t%016x\n", creds.EffectiveCaps)
	fmt.Fprintf(buf, "CapBnd:\t%016x\n", creds.BoundingCaps)
	fmt.Fprintf(buf, "Seccomp:\t%d\n", s.task.SeccompMode())
	// We unconditionally report a single NUMA node. See
	// pkg/sentry/syscalls/linux/sys_mempolicy.go.
	fmt.Fprintf(buf, "Mems_allowed:\t1\n")
	fmt.Fprintf(buf, "Mems_allowed_list:\t0\n")
	return nil
}

// ioUsage is the /proc/<pid>/io and /proc/<pid>/task/<tid>/io data provider.
type ioUsage interface {
	// IOUsage returns the io usage data.
	IOUsage() *usage.IO
}

// +stateify savable
type ioData struct {
	kernfs.DynamicBytesFile

	ioUsage
}

var _ dynamicInode = (*ioData)(nil)

// Generate implements vfs.DynamicBytesSource.Generate.
func (i *ioData) Generate(ctx context.Context, buf *bytes.Buffer) error {
	io := usage.IO{}
	io.Accumulate(i.IOUsage())

	fmt.Fprintf(buf, "char: %d\n", io.CharsRead)
	fmt.Fprintf(buf, "wchar: %d\n", io.CharsWritten)
	fmt.Fprintf(buf, "syscr: %d\n", io.ReadSyscalls)
	fmt.Fprintf(buf, "syscw: %d\n", io.WriteSyscalls)
	fmt.Fprintf(buf, "read_bytes: %d\n", io.BytesRead)
	fmt.Fprintf(buf, "write_bytes: %d\n", io.BytesWritten)
	fmt.Fprintf(buf, "cancelled_write_bytes: %d\n", io.BytesWriteCancelled)
	return nil
}

// oomScoreAdj is a stub of the /proc/<pid>/oom_score_adj file.
//
// +stateify savable
type oomScoreAdj struct {
	kernfs.DynamicBytesFile

	task *kernel.Task
}

var _ vfs.WritableDynamicBytesSource = (*oomScoreAdj)(nil)

// Generate implements vfs.DynamicBytesSource.Generate.
func (o *oomScoreAdj) Generate(ctx context.Context, buf *bytes.Buffer) error {
	adj, err := o.task.OOMScoreAdj()
	if err != nil {
		return err
	}
	fmt.Fprintf(buf, "%d\n", adj)
	return nil
}

// Write implements vfs.WritableDynamicBytesSource.Write.
func (o *oomScoreAdj) Write(ctx context.Context, src usermem.IOSequence, offset int64) (int64, error) {
	if src.NumBytes() == 0 {
		return 0, nil
	}

	// Limit input size so as not to impact performance if input size is large.
	src = src.TakeFirst(usermem.PageSize - 1)

	var v int32
	n, err := usermem.CopyInt32StringInVec(ctx, src.IO, src.Addrs, &v, src.Opts)
	if err != nil {
		return 0, err
	}

	if err := o.task.SetOOMScoreAdj(v); err != nil {
		return 0, err
	}

	return n, nil
}
