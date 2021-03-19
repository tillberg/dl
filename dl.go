package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/tillberg/alog"
	"github.com/tillberg/stringset"
)

var (
	serviceWhitelist *stringset.StringSet
	watchers         = map[string]*Watcher{}
	watchersMutex    sync.Mutex
	dockerClient     *client.Client
	maxServiceLength int
)

func main() {
	services := os.Args[1:]
	serviceWhitelist = stringset.New(services...)
	for _, service := range services {
		if len(service) > maxServiceLength {
			maxServiceLength = len(service)
		}
	}
	var err error
	dockerClient, err = client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	alog.BailIf(err)
	go watchEvents()
	go watchExisting()
	select {}
}

func watchEvents() {
	events, errs := dockerClient.Events(context.Background(), types.EventsOptions{})
	go func() {
		for err := range errs {
			alog.Printf("Docker reported error: %v\n", err)
			alog.BailIf(err)
		}
	}()
	for ev := range events {
		switch ev.Type {
		case "container":
			containerEvent(ev)
		}
	}
}

func watchExisting() {
	containers, err := dockerClient.ContainerList(context.Background(), types.ContainerListOptions{})
	alog.BailIf(err)
	for _, container := range containers {
		service := container.Labels["com.docker.compose.service"]
		containerStart(service, container.ID, true, time.Now().Add(-1*time.Second))
	}
}

func containerEvent(ev events.Message) {
	service := ev.Actor.Attributes["com.docker.compose.service"]
	switch ev.Action {
	case "start":
		containerStart(service, ev.ID, false, time.Unix(ev.Time, 0))
	case "stop", "die":
		containerStop(service, ev.ID)
	default:
		// alog.Printf("@(dim:unhandled container event for %s %s: %s)\n", service, ev.ID, ev.Action)
	}
}

func containerStart(service string, id string, onStartup bool, startTime time.Time) {
	if !serviceWhitelist.Has(service) {
		return
	}
	lg := alog.New(os.Stderr, getServiceLogPrefix(service), 0)
	if onStartup {
		lg.Printf("@(green:watching) @(dim:%s)\n", id)
	} else {
		lg.Printf("@(green:started) @(dim:%s)\n", id)
	}
	watchersMutex.Lock()
	defer watchersMutex.Unlock()
	w, exists := watchers[id]
	if !exists {
		w = NewWatcher(service, id)
		watchers[id] = w
	}
	w.start(startTime)
}

func containerStop(service string, id string) {
	if !serviceWhitelist.Has(service) {
		return
	}
	lg := alog.New(os.Stderr, getServiceLogPrefix(service), 0)
	lg.Printf("@(red:stopped) @(dim:%s)\n", id)
	watchersMutex.Lock()
	defer watchersMutex.Unlock()
	_, exists := watchers[id]
	if !exists {
		return
	}
	w := watchers[id]
	w.stop()
}

func getServiceLogPrefix(service string) string {
	var prefix string
	if serviceWhitelist.Len() > 1 {
		paddedService := strings.Repeat(" ", maxServiceLength-len(service)) + service
		prefix = fmt.Sprintf(alog.Colorify("@(cyan:%s) "), paddedService)
	}
	return prefix
}

type Watcher struct {
	Service              string
	ContainerID          string
	cancel               context.CancelFunc
	wasStartedPreviously bool
}

func NewWatcher(service string, containerID string) *Watcher {
	return &Watcher{
		Service:     service,
		ContainerID: containerID,
	}
}

func (w *Watcher) start(startTime time.Time) {
	if w.cancel != nil {
		// already running
		return
	}
	wasStartedPreviously := w.wasStartedPreviously
	var ctx context.Context
	ctx, w.cancel = context.WithCancel(context.Background())
	go func() {
		defer w.cancel()
		w.run(ctx, startTime, wasStartedPreviously)
	}()
	w.wasStartedPreviously = true
}

func (w *Watcher) run(ctx context.Context, startTime time.Time, wasStartedPreviously bool) {
	logsOpts := types.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	}
	if wasStartedPreviously {
		// This is far from perfect, but docker's API kind of sucks and this is pretty OK.
		// When a container dies, ContainerLogs stops sending new logs. When the container
		// is restarted, we get a new "start" event, but a new invocation of ContainerLogs
		// can return logs from before the container died/stopped. So we try to filter the
		// logs since the start time, but since this filter only has resolution in seconds
		// and doesn't appear to strictly order the start event and first log message after
		// start, we sometimes repeat log messages that were emitted just before the stop.
		// :shrug:
		logsOpts.Since = startTime.Add(-time.Second).UTC().Format("2006-01-02T15:04:05Z")
	} else {
		logsOpts.Tail = "1000"
	}
	logsReader, err := dockerClient.ContainerLogs(ctx, w.ContainerID, logsOpts)
	if err != nil && strings.HasSuffix(err.Error(), context.Canceled.Error()) {
		return
	}
	alog.BailIf(err)
	lgOut := alog.New(os.Stderr, getServiceLogPrefix(w.Service), 0)
	lgErr := alog.New(os.Stderr, getServiceLogPrefix(w.Service), 0)
	_, err = stdcopy.StdCopy(lgOut, lgErr, logsReader)
	if err == context.Canceled {
		return
	}
	if err != nil {
		alog.Printf("@(warn:stdcopy.StdCopy failed: %v)\n", err)
		return
	}
}

func (w *Watcher) stop() {
	if w.cancel == nil {
		// not running
		return
	}
	w.cancel()
	w.cancel = nil
}
