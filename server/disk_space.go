package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
	"shelley.exe.dev/server/diskspace"
	"shelley.exe.dev/subpub"
)

const (
	diskSpaceSettingKey = "internal.disk_space"
)

// Persist only episode transitions and dismissal, never periodic measurements.
type diskSpaceEpisode struct {
	EpisodeID uint64 `json:"episode_id"`
	Revision  uint64 `json:"revision"`
	Active    bool   `json:"active"`
	Critical  bool   `json:"critical"`
	Dismissed bool   `json:"dismissed"`
}

type diskSpaceMonitor struct {
	mu     sync.Mutex // serializes observations, persistence, publishing and subscription
	server *Server
	probe  func(string) (available, total uint64, err error)
	status diskspace.DiskSpaceStatus
	// unsaved means status is newer than the persisted episode, typically
	// because the disk was too full to write it. Every check retries.
	unsaved bool
	// poking guards the single background check queued by poke.
	poking atomic.Bool
}

func diskBytes(path string) (available, total uint64, err error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, 0, fmt.Errorf("statfs %q: %w", path, err)
	}
	return stat.Bavail * uint64(stat.Bsize), stat.Blocks * uint64(stat.Bsize), nil
}

// initDiskSpace runs synchronously before serving. Route-only tests can inject
// a probe here and call check directly, without a worker or any sleeps.
func (s *Server) initDiskSpace(ctx context.Context, probe func(string) (available, total uint64, err error)) error {
	value, err := s.db.GetSetting(ctx, diskSpaceSettingKey)
	if err != nil {
		return fmt.Errorf("load disk space episode: %w", err)
	}
	var episode diskSpaceEpisode
	if value != "" {
		if err := json.Unmarshal([]byte(value), &episode); err != nil {
			return fmt.Errorf("decode disk space episode: %w", err)
		}
	}
	m := &diskSpaceMonitor{
		server: s,
		probe:  probe,
		status: diskspace.DiskSpaceStatus{
			EpisodeID: episode.EpisodeID,
			Revision:  episode.Revision,
			Active:    episode.Active,
			Critical:  episode.Critical,
			Dismissed: episode.Dismissed,
		},
	}
	if err := m.check(ctx); err != nil {
		return err
	}
	s.diskSpace.Store(m)
	return nil
}

// onDiskFull runs when a database operation fails because the disk is full.
// The failure is the most timely signal there is, so re-check right away
// rather than waiting for the next turn end or page load.
func (s *Server) onDiskFull() {
	if m := s.diskSpace.Load(); m != nil {
		m.poke()
	}
}

// poke checks in the background: the hook runs on the failing caller's
// goroutine, possibly while check itself holds mu (its own write can fail
// too). At most one poke runs at a time; extra pokes are dropped.
func (m *diskSpaceMonitor) poke() {
	if !m.poking.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer m.poking.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := m.check(ctx); err != nil {
			m.server.logger.Error("Disk space check failed", "error", err)
		}
	}()
}

func (m *diskSpaceMonitor) snapshot() diskspace.DiskSpaceStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func (m *diskSpaceMonitor) check(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	available, total, err := m.probe(m.server.db.Path())
	if err != nil {
		// Unknown is not recovery: retain the last successful observation.
		return err
	}
	next := m.status
	next.AvailableBytes = available
	next.TotalBytes = total
	next.Active = available < diskspace.Threshold
	// Critical latches within an episode: hovering around the line must not
	// re-show the notice, so only recovery clears it.
	next.Critical = next.Active && (m.status.Critical || available < diskspace.CriticalThreshold)
	if next.Active == m.status.Active && next.Critical == m.status.Critical {
		m.status = next
		// Visible notice: push the fresh number. Same revision, nothing persisted.
		if next.Active && !next.Dismissed {
			m.server.streamPub.Broadcast(StreamResponse{DiskSpaceStatus: &next})
		}
	} else {
		// Entering low, entering critical or recovering each un-dismiss: the
		// user gets one fresh notice per escalation.
		next.Dismissed = false
		if next.Active && !m.status.Active {
			next.EpisodeID++
		}
		next.Revision++
		// Publish before persisting: a full disk is exactly when the write
		// fails, and that must not hide the notice saying so.
		m.status = next
		m.unsaved = true
		m.server.streamPub.Broadcast(StreamResponse{DiskSpaceStatus: &next})
	}
	if m.unsaved {
		if err := m.persist(ctx, m.status); err != nil {
			// Not fatal: the persisted episode only matters across restarts.
			m.server.logger.Error("Failed to persist disk space notice", "error", err)
		} else {
			m.unsaved = false
		}
	}
	return nil
}

// persist requires mu.
func (m *diskSpaceMonitor) persist(ctx context.Context, status diskspace.DiskSpaceStatus) error {
	value, err := json.Marshal(diskSpaceEpisode{
		EpisodeID: status.EpisodeID,
		Revision:  status.Revision,
		Active:    status.Active,
		Critical:  status.Critical,
		Dismissed: status.Dismissed,
	})
	if err != nil {
		return err
	}
	if err := m.server.db.SetSetting(ctx, diskSpaceSettingKey, string(value)); err != nil {
		return fmt.Errorf("persist disk space episode: %w", err)
	}
	return nil
}

// dismiss is durable or fails: the user is told when it did not stick.
func (m *diskSpaceMonitor) dismiss(ctx context.Context, episodeID uint64) (diskspace.DiskSpaceStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.status.Active && !m.status.Dismissed && m.status.EpisodeID == episodeID {
		next := m.status
		next.Dismissed = true
		next.Revision++
		if err := m.persist(ctx, next); err != nil {
			return m.status, err
		}
		m.status = next
		m.unsaved = false
		m.server.streamPub.Broadcast(StreamResponse{DiskSpaceStatus: &next})
	}
	return m.status, nil
}

// refreshDiskSpace runs when an agent turn ends: that is when the disk most
// likely just changed. Together with the startup check this is the only
// sampling; one statfs, no DB write or broadcast unless the threshold was
// crossed.
func (s *Server) refreshDiskSpace(ctx context.Context) {
	m := s.diskSpace.Load()
	if m == nil {
		return
	}
	if err := m.check(ctx); err != nil {
		s.logger.Error("Disk space check failed", "error", err)
	}
}

// Sample once per new stream (a page load or reconnect), so someone opening an
// idle Shelley sees the current state, then subscribe and capture the snapshot
// under the publishing lock. Any transition the sample caused was broadcast
// before this subscriber existed, so it arrives only via the snapshot; events
// queued afterwards are never older than it.
func (s *Server) subscribeStream(ctx context.Context) (func() (StreamResponse, bool), *subpub.SubscriptionStatus, *diskspace.DiskSpaceStatus) {
	s.refreshDiskSpace(ctx)
	var snapshot *diskspace.DiskSpaceStatus
	if m := s.diskSpace.Load(); m != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		current := m.status
		snapshot = &current
	}
	next, status := s.streamPub.SubscribeWithStatus(ctx, -1)
	return next, status, snapshot
}

func (s *Server) handleDismissDiskSpace(w http.ResponseWriter, r *http.Request) {
	var req struct {
		EpisodeID uint64 `json:"episode_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.EpisodeID == 0 {
		http.Error(w, "A positive episode_id is required", http.StatusBadRequest)
		return
	}
	m := s.diskSpace.Load()
	if m == nil {
		http.Error(w, "Disk space monitor not initialized", http.StatusServiceUnavailable)
		return
	}
	status, err := m.dismiss(r.Context(), req.EpisodeID)
	if err != nil {
		s.internalError(w, "Failed to dismiss disk space notice", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}
