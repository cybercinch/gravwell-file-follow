// Package docker connects to a container runtime via the Docker-compatible API
// and drives config generation.  Both Docker and Podman are supported; the
// backend is selected via config.Config.Backend.
//
// Responsibilities:
//   - Initial sync: enumerate all running containers and write overlay configs.
//   - Event loop: react to container start / die / destroy events.
//   - Debounce: coalesce rapid events (e.g. stack up/down) into a single
//     gravwell_file_follow restart.
//   - Reconnect: back off and reconnect if the API stream drops.
package docker

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	dockerclient "github.com/docker/docker/client"

	"github.com/cybercinch/docker-log-watcher/internal/config"
	"github.com/cybercinch/docker-log-watcher/internal/follower"
	"github.com/cybercinch/docker-log-watcher/internal/supervisor"
)

const (
	labelSkip       = "gravwell.skip"
	labelTag        = "gravwell.tag"
	labelEnable     = "gravwell.enable"
	labelLogDir          = "gravwell.log-dir"
	labelLogFilter       = "gravwell.log-filter"
	labelPersist         = "gravwell.persist"
	labelPersistTTL      = "gravwell.persist-ttl"
	labelTimestampRegex  = "gravwell.timestamp-regex"
	labelTimestampFormat = "gravwell.timestamp-format"

	reconnectBaseDelay = 2 * time.Second
	reconnectMaxDelay  = 60 * time.Second
)

// containerMeta holds the resolved properties of a single container.
type containerMeta struct {
	name, tag, logDir, fileFilter   string
	timestampRegex, timestampFormat string
	persist                         bool
	persistTTL                      time.Duration
	skip                            bool
}

// Watcher drives the Docker event loop.
type Watcher struct {
	cfg  config.Config
	gen  *follower.Generator
	sup  *supervisor.Supervisor

	// dirty is set when at least one config has changed since the last restart.
	dirty bool

	// debounceTimer fires after cfg.DebounceDuration to trigger a restart.
	debounceTimer *time.Timer

	// pendingDestroys tracks delayed config removals (drain delay) keyed by
	// sanitised container name.  Cancelled when the container restarts.
	// pendingExpiries tracks TTL-based removals for persisted configs.
	// Both maps are protected by mu.
	mu              sync.Mutex
	pendingDestroys map[string]*time.Timer
	pendingExpiries map[string]*time.Timer
}

// New creates a Watcher.
func New(cfg config.Config, gen *follower.Generator, sup *supervisor.Supervisor) *Watcher {
	return &Watcher{
		cfg:             cfg,
		gen:             gen,
		sup:             sup,
		pendingDestroys: make(map[string]*time.Timer),
		pendingExpiries: make(map[string]*time.Timer),
	}
}

// newClient creates a Docker-compatible API client.  When socketPath is
// non-empty it is used directly (supports both Docker and Podman sockets);
// otherwise the client falls back to standard Docker env resolution
// (DOCKER_HOST / default socket).
func newClient(socketPath string) (*dockerclient.Client, error) {
	opts := []dockerclient.Opt{dockerclient.WithAPIVersionNegotiation()}
	if socketPath != "" {
		opts = append(opts, dockerclient.WithHost("unix://"+socketPath))
	} else {
		opts = append(opts, dockerclient.FromEnv)
	}
	return dockerclient.NewClientWithOpts(opts...)
}

// Run performs the initial sync then blocks on the container runtime event
// stream.  It returns only when ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) error {
	socket := w.cfg.ResolvedSocketPath()
	cli, err := newClient(socket)
	if err != nil {
		return err
	}
	defer cli.Close()

	// ── Initial sync ─────────────────────────────────────────────────────────
	slog.Info("performing initial container sync", "backend", w.cfg.Backend)
	if err := w.syncAll(ctx, cli); err != nil {
		slog.Warn("initial sync failed", "err", err)
	}

	// Start file_follow with whatever configs exist right now.
	if err := w.sup.Start(); err != nil {
		return err
	}

	// ── Event loop ───────────────────────────────────────────────────────────
	delay := reconnectBaseDelay
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := w.runEventStream(ctx, cli)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("runtime event stream disconnected, reconnecting",
			"backend", w.cfg.Backend, "err", err, "in", delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, reconnectMaxDelay)
		// Reconnect.
		cli.Close()
		cli, err = newClient(socket)
		if err != nil {
			slog.Error("failed to reconnect to runtime", "backend", w.cfg.Backend, "err", err)
			continue
		}
		delay = reconnectBaseDelay
	}
}

// runEventStream subscribes to Docker container events and handles them until
// the connection drops or ctx is cancelled.
func (w *Watcher) runEventStream(ctx context.Context, cli *dockerclient.Client) error {
	f := filters.NewArgs(
		filters.Arg("type", "container"),
		filters.Arg("event", "start"),
		filters.Arg("event", "die"),
		filters.Arg("event", "destroy"),
	)
	eventsCh, errCh := cli.Events(ctx, events.ListOptions{Filters: f})

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case err := <-errCh:
			return err

		case ev := <-eventsCh:
			w.handleEvent(ctx, cli, ev)
		}
	}
}

// handleEvent routes a single Docker event to the appropriate handler.
func (w *Watcher) handleEvent(ctx context.Context, cli *dockerclient.Client, ev events.Message) {
	switch ev.Action {
	case "start":
		meta := w.resolveContainer(ctx, cli, ev.Actor.ID)
		if meta.skip {
			slog.Debug("skipping container (start)", "name", meta.name)
			return
		}

		// Cancel any pending destroy or TTL expiry for this container — it's back.
		w.cancelPending(meta.name)

		path, err := w.gen.Write(ev.Actor.ID, meta.name, meta.tag, meta.logDir, meta.fileFilter, meta.persist, meta.persistTTL, meta.timestampRegex, meta.timestampFormat)
		if err != nil {
			slog.Error("failed to write config", "name", meta.name, "err", err)
			return
		}
		slog.Info("wrote config", "path", path, "tag", meta.tag)
		w.markDirty()

	case "die":
		// Leave the config in place — logs still exist and file_follow is still
		// tailing them.  We only clean up on destroy.
		name := containerNameFromActor(ev)
		if w.gen.Exists(name) {
			slog.Debug("container died, keeping config", "name", name)
		}

	case "destroy":
		name := containerNameFromActor(ev)
		if !w.gen.Exists(name) {
			slog.Debug("destroy event for untracked container, ignoring", "name", name)
			return
		}
		w.scheduleDestroy(name)
	}
}

// scheduleDestroy queues a delayed config removal for the named container.
// If DestroyDelay is 0 the removal happens immediately.
func (w *Watcher) scheduleDestroy(name string) {
	if w.cfg.DestroyDelay <= 0 {
		w.doRemove(name)
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if t, ok := w.pendingDestroys[name]; ok {
		t.Stop()
	}

	slog.Info("scheduling config removal after drain delay",
		"name", name, "delay", w.cfg.DestroyDelay)

	w.pendingDestroys[name] = time.AfterFunc(w.cfg.DestroyDelay, func() {
		w.mu.Lock()
		delete(w.pendingDestroys, name)
		w.mu.Unlock()
		w.doRemove(name)
	})
}

// cancelPending cancels any pending drain-delay removal or TTL expiry for name
// and clears the .expiry file so the TTL resets on the next destroy.
func (w *Watcher) cancelPending(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if t, ok := w.pendingDestroys[name]; ok {
		t.Stop()
		delete(w.pendingDestroys, name)
		slog.Info("cancelled pending drain removal (container restarted)", "name", name)
	}
	if t, ok := w.pendingExpiries[name]; ok {
		t.Stop()
		delete(w.pendingExpiries, name)
		slog.Info("cancelled pending TTL expiry (container restarted)", "name", name)
	}
}

// doRemove is called after the drain delay.  For persisted configs it either
// keeps the config forever (no TTL) or schedules a TTL-based expiry timer and
// writes the absolute expiry to disk so it survives watcher restarts.
// For non-persisted configs it removes immediately.
func (w *Watcher) doRemove(name string) {
	if w.gen.IsPersisted(name) {
		ttl := w.gen.PersistTTL(name)
		if ttl <= 0 {
			slog.Info("container destroyed, keeping persisted config (no TTL)", "name", name)
			return
		}
		expiry := time.Now().Add(ttl)
		if err := w.gen.SetExpiry(name, expiry); err != nil {
			slog.Warn("failed to write expiry file, config will persist forever", "name", name, "err", err)
			return
		}
		w.scheduleExpiry(name, expiry)
		slog.Info("container destroyed, config will expire", "name", name, "expiry", expiry.Format(time.RFC3339))
		return
	}
	w.doActualRemove(name)
}

// scheduleExpiry sets a timer that fires at expiry and removes the config.
// mu must NOT be held by the caller.
func (w *Watcher) scheduleExpiry(name string, expiry time.Time) {
	remaining := time.Until(expiry)
	if remaining <= 0 {
		w.doActualRemove(name)
		return
	}
	w.mu.Lock()
	if t, ok := w.pendingExpiries[name]; ok {
		t.Stop()
	}
	w.pendingExpiries[name] = time.AfterFunc(remaining, func() {
		w.mu.Lock()
		delete(w.pendingExpiries, name)
		w.mu.Unlock()
		w.doActualRemove(name)
	})
	w.mu.Unlock()
}

// doActualRemove unconditionally removes the config and marks the watcher
// dirty.  It is the final step of both the non-persisted and TTL expiry paths.
func (w *Watcher) doActualRemove(name string) {
	removed, err := w.gen.Remove(name)
	if err != nil {
		slog.Error("failed to remove config", "name", name, "err", err)
		return
	}
	if !removed {
		slog.Debug("config already gone for container", "name", name)
		return
	}
	slog.Info("removed config", "name", name)
	w.markDirty()
}

// syncAll enumerates all currently running containers and writes configs for
// any that are not already present.  It also removes stale configs for
// containers that are no longer running or are now excluded, and resumes any
// in-flight TTL expiry timers for persisted configs whose .expiry file survived
// a watcher restart.
func (w *Watcher) syncAll(ctx context.Context, cli *dockerclient.Client) error {
	containers, err := cli.ContainerList(ctx, container.ListOptions{All: false})
	if err != nil {
		return err
	}

	// Build a set of sanitised names for containers that SHOULD have configs.
	wanted := make(map[string]struct{})

	for _, ctr := range containers {
		meta := w.resolveContainerFromList(ctr.Names, ctr.Labels, ctr.ID)
		if meta.skip {
			slog.Debug("skipping container (sync)", "name", meta.name)
			continue
		}
		wanted[follower.SanitizeName(meta.name)] = struct{}{}
		// Always rewrite — ensures Base-Directory reflects the current
		// ContainerDir and picks up new container IDs for recreated containers.
		path, err := w.gen.Write(ctr.ID, meta.name, meta.tag, meta.logDir, meta.fileFilter, meta.persist, meta.persistTTL, meta.timestampRegex, meta.timestampFormat)
		if err != nil {
			slog.Error("failed to write config during sync", "name", meta.name, "err", err)
			continue
		}
		slog.Info("synced config", "path", path, "tag", meta.tag)
		w.dirty = true
	}

	// ── Cleanup / expiry resumption ──────────────────────────────────────────
	existing, err := w.gen.ListConfigs()
	if err != nil {
		slog.Warn("failed to list existing configs for cleanup", "err", err)
		return nil
	}
	for _, safeName := range existing {
		if _, ok := wanted[safeName]; ok {
			continue // running container — nothing to do
		}
		if !w.gen.IsPersisted(safeName) {
			slog.Info("removing stale/excluded config", "name", safeName)
			if _, err := w.gen.Remove(safeName); err != nil {
				slog.Error("failed to remove stale config", "name", safeName, "err", err)
			}
			w.dirty = true
			continue
		}
		// Persisted and not running — resume TTL expiry timer if one was set.
		if expiry, ok := w.gen.ReadExpiry(safeName); ok {
			if time.Now().After(expiry) {
				slog.Info("persisted config past expiry, removing", "name", safeName)
				w.doActualRemove(safeName)
			} else {
				slog.Info("resuming TTL expiry timer", "name", safeName, "expiry", expiry.Format(time.RFC3339))
				w.scheduleExpiry(safeName, expiry)
			}
		} else {
			slog.Debug("keeping persisted config for non-running container", "name", safeName)
		}
	}

	return nil
}

// resolveContainer inspects a container by ID to populate a containerMeta.
func (w *Watcher) resolveContainer(ctx context.Context, cli *dockerclient.Client, id string) containerMeta {
	info, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		slog.Warn("failed to inspect container", "id", id, "err", err)
		return containerMeta{name: id, tag: w.cfg.DefaultTag}
	}

	name := strings.TrimPrefix(info.Name, "/")
	labels := info.Config.Labels

	if labels[labelSkip] == "true" {
		return containerMeta{name: name, skip: true}
	}
	if w.cfg.ShouldExclude(name) {
		return containerMeta{name: name, skip: true}
	}
	if w.cfg.OptIn && labels[labelEnable] != "true" {
		return containerMeta{name: name, skip: true}
	}

	tag := labels[labelTag]
	if tag == "" {
		tag = follower.TagFromName(name)
		if tag == "" {
			tag = w.cfg.DefaultTag
		}
	}

	persistTTL, _ := time.ParseDuration(labels[labelPersistTTL])

	return containerMeta{
		name:            name,
		tag:             tag,
		logDir:          labels[labelLogDir],
		fileFilter:      labels[labelLogFilter],
		persist:         labels[labelPersist] == "true",
		persistTTL:      persistTTL,
		timestampRegex:  labels[labelTimestampRegex],
		timestampFormat: labels[labelTimestampFormat],
	}
}

// resolveContainerFromList populates a containerMeta from the ContainerList
// payload (no extra API call needed).
func (w *Watcher) resolveContainerFromList(names []string, labels map[string]string, id string) containerMeta {
	name := id[:12]
	if len(names) > 0 {
		name = strings.TrimPrefix(names[0], "/")
	}

	if labels[labelSkip] == "true" {
		return containerMeta{name: name, skip: true}
	}
	if w.cfg.ShouldExclude(name) {
		return containerMeta{name: name, skip: true}
	}
	if w.cfg.OptIn && labels[labelEnable] != "true" {
		return containerMeta{name: name, skip: true}
	}

	tag := labels[labelTag]
	if tag == "" {
		tag = follower.TagFromName(name)
		if tag == "" {
			tag = w.cfg.DefaultTag
		}
	}

	persistTTL, _ := time.ParseDuration(labels[labelPersistTTL])

	return containerMeta{
		name:            name,
		tag:             tag,
		logDir:          labels[labelLogDir],
		fileFilter:      labels[labelLogFilter],
		persist:         labels[labelPersist] == "true",
		persistTTL:      persistTTL,
		timestampRegex:  labels[labelTimestampRegex],
		timestampFormat: labels[labelTimestampFormat],
	}
}

// containerNameFromActor extracts the container name from an event's Actor
// attributes map (populated on destroy events).
func containerNameFromActor(ev events.Message) string {
	if n, ok := ev.Actor.Attributes["name"]; ok {
		return strings.TrimPrefix(n, "/")
	}
	return ev.Actor.ID[:12]
}

// markDirty notes that a config changed and (re)starts the debounce timer.
// When the timer fires, gravwell_file_follow is restarted exactly once.
// mu must NOT be held by the caller.
func (w *Watcher) markDirty() {
	w.mu.Lock()
	w.dirty = true
	if w.debounceTimer != nil {
		w.debounceTimer.Stop()
	}
	w.debounceTimer = time.AfterFunc(w.cfg.DebounceDuration, func() {
		w.mu.Lock()
		if !w.dirty {
			w.mu.Unlock()
			return
		}
		w.dirty = false
		w.mu.Unlock()

		slog.Info("debounce window closed, restarting file_follow")
		if err := w.sup.Restart(); err != nil {
			slog.Error("failed to restart file_follow", "err", err)
		}
	})
	w.mu.Unlock()
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
