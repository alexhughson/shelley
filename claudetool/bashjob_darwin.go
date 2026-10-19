package claudetool

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func darwinProcessInfo(pid int) (*unix.KinfoProc, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return nil, fmt.Errorf("sysctl kern.proc.pid %d: %w", pid, err)
	}
	if len(procs) == 0 {
		return nil, ErrBackgroundJobGone
	}
	if len(procs) != 1 {
		return nil, fmt.Errorf("sysctl kern.proc.pid %d returned %d processes", pid, len(procs))
	}
	return &procs[0], nil
}

func darwinStartTime(info *unix.KinfoProc) uint64 {
	tv := info.Proc.P_starttime
	return uint64(tv.Sec)*1_000_000 + uint64(tv.Usec)
}

// processStartTime returns the process's wall-clock start time in microseconds.
// The start time survives exec and is available for an unreaped zombie.
func processStartTime(pid int) (uint64, error) {
	info, err := darwinProcessInfo(pid)
	if err != nil {
		return 0, err
	}
	return darwinStartTime(info), nil
}

// Exited watches the wrapper with NOTE_EXIT, which also supports nonchildren.
// A missing process or reused PID yields an already-closed channel.
func (j BackgroundJob) Exited() (<-chan struct{}, error) {
	exited := make(chan struct{})
	if j.PID <= 1 {
		close(exited)
		return exited, nil
	}
	fd, err := unix.Kqueue()
	if err != nil {
		return nil, fmt.Errorf("kqueue: %w", err)
	}
	change := []unix.Kevent_t{{
		Ident:  uint64(j.PID),
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_ONESHOT,
		Fflags: unix.NOTE_EXIT,
	}}
	for {
		// With no event buffer this only registers the filter; registration
		// errors are returned directly rather than as EV_ERROR events.
		_, err = unix.Kevent(fd, change, nil, nil)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		unix.Close(fd)
		if errors.Is(err, unix.ESRCH) {
			close(exited)
			return exited, nil
		}
		return nil, fmt.Errorf("watch process %d: %w", j.PID, err)
	}
	// Validate after registration: a check before it could watch a replacement
	// process if the wrapper exited and its PID was reused between the calls.
	info, err := darwinProcessInfo(j.PID)
	if errors.Is(err, ErrBackgroundJobGone) || err == nil && darwinStartTime(info) != j.StartTime {
		unix.Close(fd)
		close(exited)
		return exited, nil
	}
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	go func() {
		defer unix.Close(fd)
		events := make([]unix.Kevent_t, 1)
		for {
			n, err := unix.Kevent(fd, nil, events, nil)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				// This API cannot report errors after returning its channel.
				panic(fmt.Errorf("wait for process %d: %w", j.PID, err))
			}
			if n == 0 {
				continue
			}
			event := events[0]
			if event.Flags&unix.EV_ERROR != 0 {
				panic(fmt.Errorf("wait for process %d: %w", j.PID, unix.Errno(event.Data)))
			}
			if event.Ident == uint64(j.PID) && event.Filter == unix.EVFILT_PROC && event.Fflags&unix.NOTE_EXIT != 0 {
				close(exited)
				return
			}
		}
	}()
	return exited, nil
}

// leader fails with ErrBackgroundJobGone unless j's wrapper is still
// running and leads its process group.
func (j BackgroundJob) leader() error {
	if j.PID <= 1 {
		return ErrBackgroundJobGone
	}
	info, err := darwinProcessInfo(j.PID)
	if err != nil {
		return err
	}
	const szomb = 5 // Darwin's SZOMB: exited, awaiting collection by its parent.
	if darwinStartTime(info) != j.StartTime || info.Proc.P_stat == szomb || int(info.Eproc.Pgid) != j.PID {
		return ErrBackgroundJobGone
	}
	return nil
}

// Kill sends SIGTERM to the job's process group after checking its leader.
func (j BackgroundJob) Kill() error {
	// kill(0) signals our group; kill(-1) signals every permitted process,
	// so leader rejects PIDs <= 1.
	if err := j.leader(); err != nil {
		return err
	}
	// Darwin has no identity-bound process-group signal API: the numeric PGID
	// can still be reused between this validation and kill. A kqueue watch does
	// not reserve the PID or PGID and would not eliminate that race.
	err := unix.Kill(-j.PID, unix.SIGTERM)
	if errors.Is(err, unix.ESRCH) {
		return ErrBackgroundJobGone
	}
	return err
}
