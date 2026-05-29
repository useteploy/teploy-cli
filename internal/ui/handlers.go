package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/caddy"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/deploy"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/env"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// withApp validates the app name path parameter, resolves the server, and
// provides an SSH executor to the handler. The executor comes from the connection pool.
func (s *Server) withApp(r *http.Request) (ssh.Executor, string, error) {
	appName := r.PathValue("name")
	if err := validateAppName(appName); err != nil {
		return nil, "", err
	}

	// Find which server this app is on by scanning all servers.
	serversPath, err := config.DefaultServersPath()
	if err != nil {
		return nil, "", fmt.Errorf("resolving servers path: %w", err)
	}

	servers, err := config.ListServers(serversPath)
	if err != nil {
		return nil, "", fmt.Errorf("listing servers: %w", err)
	}

	// Try each server to find where this app is deployed.
	ctx := r.Context()
	for serverName := range servers {
		exec, err := s.pool.Get(ctx, serverName)
		if err != nil {
			continue
		}
		// Check if this app exists on this server.
		out, err := exec.Run(ctx, fmt.Sprintf("test -d /deployments/%s && echo yes", shellQuote(appName)))
		if err == nil && strings.TrimSpace(out) == "yes" {
			return exec, appName, nil
		}
	}

	return nil, "", fmt.Errorf("app %q not found on any server", appName)
}

// --- Server handlers ---

func (s *Server) handleListServers(w http.ResponseWriter, r *http.Request) {
	serversPath, err := config.DefaultServersPath()
	if err != nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}

	servers, err := config.ListServers(serversPath)
	if err != nil {
		// No servers.yml yet — return empty list, not an error
		writeJSON(w, http.StatusOK, []any{})
		return
	}

	type serverInfo struct {
		Name   string            `json:"name"`
		Host   string            `json:"host"`
		User   string            `json:"user"`
		Role   string            `json:"role"`
		Tags   map[string]string `json:"tags,omitempty"`
		Online bool              `json:"online"`
	}

	result := make([]serverInfo, 0)
	for name, srv := range servers {
		user := srv.User
		if user == "" {
			user = "root"
		}
		role := srv.Role
		if role == "" {
			role = "app"
		}
		info := serverInfo{
			Name: name,
			Host: srv.Host,
			User: user,
			Role: role,
			Tags: srv.Tags,
		}
		// Quick connectivity check with 5s timeout to avoid blocking on unreachable hosts.
		checkCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		exec, err := s.pool.Get(checkCtx, name)
		if err == nil {
			_, err = exec.Run(checkCtx, "echo ok")
			info.Online = err == nil
		}
		cancel()
		result = append(result, info)
	}

	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleServerStatus(w http.ResponseWriter, r *http.Request) {
	serverName := r.PathValue("name")
	if err := validateServerName(serverName); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	exec, err := s.pool.Get(r.Context(), serverName)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("cannot connect to %s: %v", serverName, err))
		return
	}

	ctx := r.Context()

	type serverStatus struct {
		Name       string             `json:"name"`
		Host       string             `json:"host"`
		Uptime     string             `json:"uptime"`
		CPULoad    string             `json:"cpu_load"`
		MemTotal   string             `json:"mem_total"`
		MemUsed    string             `json:"mem_used"`
		MemPercent string             `json:"mem_percent"`
		DiskTotal  string             `json:"disk_total"`
		DiskUsed   string             `json:"disk_used"`
		DiskPercent string            `json:"disk_percent"`
		Containers []docker.Container `json:"containers"`
	}

	status := serverStatus{
		Name: serverName,
		Host: exec.Host(),
	}

	// Uptime
	if out, err := exec.Run(ctx, "uptime -p 2>/dev/null || uptime"); err == nil {
		status.Uptime = strings.TrimSpace(out)
	}

	// CPU load
	if out, err := exec.Run(ctx, "cat /proc/loadavg 2>/dev/null"); err == nil {
		parts := strings.Fields(out)
		if len(parts) >= 3 {
			status.CPULoad = strings.Join(parts[:3], " ")
		}
	}

	// Memory
	if out, err := exec.Run(ctx, "free -m 2>/dev/null | grep Mem"); err == nil {
		parts := strings.Fields(out)
		if len(parts) >= 3 {
			status.MemTotal = parts[1] + "M"
			status.MemUsed = parts[2] + "M"
			total := parseIntSafe(parts[1])
			used := parseIntSafe(parts[2])
			if total > 0 {
				status.MemPercent = fmt.Sprintf("%.0f%%", float64(used)/float64(total)*100)
			}
		}
	}

	// Disk
	if out, err := exec.Run(ctx, "df -h / 2>/dev/null | tail -1"); err == nil {
		parts := strings.Fields(out)
		if len(parts) >= 5 {
			status.DiskTotal = parts[1]
			status.DiskUsed = parts[2]
			status.DiskPercent = parts[4]
		}
	}

	// All teploy containers on this server
	if out, err := exec.Run(ctx, "docker ps --all --filter label=teploy.app --format '{{json .}}'"); err == nil && strings.TrimSpace(out) != "" {
		if containers, err := docker.ParseContainers(out); err == nil {
			status.Containers = containers
		}
	}

	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleServerProxy(w http.ResponseWriter, r *http.Request) {
	serverName := r.PathValue("name")
	if err := validateServerName(serverName); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	exec, err := s.pool.Get(r.Context(), serverName)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("cannot connect to %s: %v", serverName, err))
		return
	}

	ctx := r.Context()

	type routeInfo struct {
		ID          string   `json:"id"`
		Domains     []string `json:"domains"`
		Handler     string   `json:"handler"`
		Upstreams   []string `json:"upstreams,omitempty"`
		Maintenance bool     `json:"maintenance"`
	}

	type proxyStatus struct {
		Running bool        `json:"running"`
		Routes  []routeInfo `json:"routes"`
	}

	status := proxyStatus{
		Routes: make([]routeInfo, 0),
	}

	// Check if Caddy is running. The admin API binds the container's loopback
	// only (never exposed off-box), so reach it via docker exec.
	if out, err := exec.Run(ctx, "docker exec caddy curl -sf http://localhost:2019/config/ 2>/dev/null && echo ok"); err == nil && strings.Contains(out, "ok") {
		status.Running = true
	}

	// Get Caddy HTTP config to extract routes
	if status.Running {
		out, err := exec.Run(ctx, "docker exec caddy curl -sf http://localhost:2019/config/apps/http 2>/dev/null")
		if err == nil && strings.TrimSpace(out) != "" {
			var httpApp caddy.HTTPApp
			if err := json.Unmarshal([]byte(out), &httpApp); err == nil {
				for _, srv := range httpApp.Servers {
					for _, route := range srv.Routes {
						ri := routeInfo{
							ID: route.ID,
						}
						// Extract domains from match
						for _, m := range route.Match {
							ri.Domains = append(ri.Domains, m.Host...)
						}
						// Extract handler type and upstreams
						for _, h := range route.Handle {
							ri.Handler = h.Handler
							if h.Handler == "static_response" && h.StatusCode == "503" {
								ri.Maintenance = true
							}
							for _, u := range h.Upstreams {
								ri.Upstreams = append(ri.Upstreams, u.Dial)
							}
						}
						status.Routes = append(status.Routes, ri)
					}
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, status)
}

// --- App handlers ---

func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	serversPath, err := config.DefaultServersPath()
	if err != nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}

	servers, err := config.ListServers(serversPath)
	if err != nil {
		// No servers.yml yet — return empty list, not an error
		writeJSON(w, http.StatusOK, []any{})
		return
	}

	type appInfo struct {
		Name    string `json:"name"`
		Server  string `json:"server"`
		Version string `json:"version"`
		Port    int    `json:"port"`
		Status  string `json:"status"` // running, stopped, unknown
	}

	type serverResult struct {
		apps []appInfo
	}

	ch := make(chan serverResult, len(servers))
	for serverName := range servers {
		serverName := serverName
		go func() {
			var result serverResult
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()

			exec, err := s.pool.Get(ctx, serverName)
			if err != nil {
				ch <- result
				return
			}

			out, err := exec.Run(ctx, "ls -1 /deployments/ 2>/dev/null")
			if err != nil {
				ch <- result
				return
			}

			dk := docker.NewClient(exec)
			for _, appName := range strings.Split(strings.TrimSpace(out), "\n") {
				appName = strings.TrimSpace(appName)
				if appName == "" || appName == "teploy.log" || appName == "caddy" || strings.HasPrefix(appName, ".") {
					continue
				}

				info := appInfo{
					Name:   appName,
					Server: serverName,
					Status: "unknown",
				}

				if st, err := state.Read(ctx, exec, appName); err == nil && st != nil {
					info.Version = st.CurrentHash
					info.Port = st.CurrentPort
				}

				if containers, err := dk.ListContainers(ctx, appName); err == nil {
					hasRunning := false
					for _, c := range containers {
						if c.State == "running" {
							hasRunning = true
							break
						}
					}
					if hasRunning {
						info.Status = "running"
					} else if len(containers) > 0 {
						info.Status = "stopped"
					}
				}

				result.apps = append(result.apps, info)
			}
			ch <- result
		}()
	}

	apps := make([]appInfo, 0)
	for range servers {
		r := <-ch
		apps = append(apps, r.apps...)
	}

	sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
	writeJSON(w, http.StatusOK, apps)
}

func (s *Server) handleAppStatus(w http.ResponseWriter, r *http.Request) {
	exec, appName, err := s.withApp(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()

	type appStatus struct {
		Name       string             `json:"name"`
		Server     string             `json:"server"`
		Version    string             `json:"version"`
		Port       int                `json:"port"`
		PrevHash   string             `json:"previous_version,omitempty"`
		Locked     bool               `json:"locked"`
		LockInfo   *state.LockInfo    `json:"lock_info,omitempty"`
		Containers []docker.Container `json:"containers"`
	}

	status := appStatus{
		Name:   appName,
		Server: exec.Host(),
	}

	// State
	if st, err := state.Read(ctx, exec, appName); err == nil && st != nil {
		status.Version = st.CurrentHash
		status.Port = st.CurrentPort
		status.PrevHash = st.PreviousHash
	}

	// Lock
	if lock, err := state.ReadLock(ctx, exec, appName); err == nil && lock != nil {
		status.Locked = true
		status.LockInfo = lock
	}

	// Containers
	dk := docker.NewClient(exec)
	if containers, err := dk.ListContainers(ctx, appName); err == nil {
		status.Containers = containers
	}

	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleGetEnv(w http.ResponseWriter, r *http.Request) {
	exec, appName, err := s.withApp(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	mgr := env.NewManager(exec)
	entries, err := mgr.List(r.Context(), appName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	type envEntry struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	result := make([]envEntry, len(entries))
	for i, e := range entries {
		result[i] = envEntry{Key: e.Key, Value: e.Value}
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleSetEnv(w http.ResponseWriter, r *http.Request) {
	exec, appName, err := s.withApp(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Validate all keys
	for key := range body {
		if err := validateEnvKey(key); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	mgr := env.NewManager(exec)
	if err := mgr.Set(r.Context(), appName, body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleUnsetEnv(w http.ResponseWriter, r *http.Request) {
	exec, appName, err := s.withApp(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	key := r.PathValue("key")
	if err := validateEnvKey(key); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	mgr := env.NewManager(exec)
	if err := mgr.Unset(r.Context(), appName, key); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleAppLog(w http.ResponseWriter, r *http.Request) {
	exec, appName, err := s.withApp(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Read teploy.log and filter for this app
	out, err := exec.Run(r.Context(), "cat /deployments/teploy.log 2>/dev/null")
	if err != nil || strings.TrimSpace(out) == "" {
		writeJSON(w, http.StatusOK, []state.LogEntry{})
		return
	}

	var entries []state.LogEntry
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry state.LogEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry.App == appName {
			entries = append(entries, entry)
		}
	}

	// Most recent first
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Timestamp.After(entries[j].Timestamp)
	})

	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) handleListAccessories(w http.ResponseWriter, r *http.Request) {
	exec, appName, err := s.withApp(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()

	// List accessory containers (named {app}-{name}, without teploy.process label)
	out, err := exec.Run(ctx, fmt.Sprintf("docker ps --all --filter name=^%s- --format '{{json .}}'", shellQuote(appName)))
	if err != nil || strings.TrimSpace(out) == "" {
		writeJSON(w, http.StatusOK, []docker.Container{})
		return
	}

	containers, err := docker.ParseContainers(out)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Filter to only accessory containers (ones without teploy.version label pattern)
	var accessories []docker.Container
	for _, c := range containers {
		// Accessory containers are named {app}-{accname}, not {app}-{process}-{version}
		nameParts := strings.Split(strings.TrimPrefix(c.Name, appName+"-"), "-")
		if len(nameParts) == 1 {
			accessories = append(accessories, c)
		}
	}

	writeJSON(w, http.StatusOK, accessories)
}

// --- App action handlers ---

func (s *Server) handleAppStop(w http.ResponseWriter, r *http.Request) {
	exec, appName, err := s.withApp(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	lc := deploy.NewLifecycle(exec, io.Discard)
	if err := lc.Stop(r.Context(), appName, 10); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func (s *Server) handleAppStart(w http.ResponseWriter, r *http.Request) {
	exec, appName, err := s.withApp(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	lc := deploy.NewLifecycle(exec, io.Discard)
	if err := lc.Start(r.Context(), appName); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
}

func (s *Server) handleAppRestart(w http.ResponseWriter, r *http.Request) {
	exec, appName, err := s.withApp(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	lc := deploy.NewLifecycle(exec, io.Discard)
	if err := lc.Restart(r.Context(), appName, 10); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "restarted"})
}

func (s *Server) handleAppRollback(w http.ResponseWriter, r *http.Request) {
	exec, appName, err := s.withApp(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// We need the domain for rollback — read it from state context
	// For now, try to find a Caddy route for this app
	ctx := r.Context()
	st, _ := state.Read(ctx, exec, appName)
	if st == nil {
		writeError(w, http.StatusBadRequest, "no deploy state found")
		return
	}

	cfg := deploy.RollbackConfig{
		App:         appName,
		Domain:      st.Domain,
		StopTimeout: 10,
	}

	if err := deploy.Rollback(ctx, exec, io.Discard, cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "rolled back"})
}

func (s *Server) handleAppLock(w http.ResponseWriter, r *http.Request) {
	exec, appName, err := s.withApp(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var body struct {
		Message string `json:"message"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	if err := state.AcquireManualLock(r.Context(), exec, appName, "ui", body.Message); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "locked"})
}

func (s *Server) handleAppUnlock(w http.ResponseWriter, r *http.Request) {
	exec, appName, err := s.withApp(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	state.ReleaseLock(r.Context(), exec, appName)
	writeJSON(w, http.StatusOK, map[string]string{"status": "unlocked"})
}

func (s *Server) handleAppMaintenance(w http.ResponseWriter, r *http.Request) {
	exec, appName, err := s.withApp(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	action := r.PathValue("action")
	if action != "on" && action != "off" {
		writeError(w, http.StatusBadRequest, "action must be 'on' or 'off'")
		return
	}

	cd := caddy.NewClient(exec)
	ctx := r.Context()

	// Read the actual domain from deploy state — Caddy routes match by domain.
	domain := appName
	if st, _ := state.Read(ctx, exec, appName); st != nil && st.Domain != "" {
		domain = st.Domain
	}

	if action == "on" {
		if err := cd.SetMaintenance(ctx, appName, domain); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		if err := cd.RemoveMaintenance(ctx, appName); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "maintenance " + action})
}

func (s *Server) handleAccessoryStart(w http.ResponseWriter, r *http.Request) {
	exec, appName, err := s.withApp(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	accName := r.PathValue("acc")
	if err := validateAppName(accName); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	containerName := appName + "-" + accName
	dk := docker.NewClient(exec)
	if err := dk.Start(r.Context(), containerName); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
}

func (s *Server) handleAccessoryStop(w http.ResponseWriter, r *http.Request) {
	exec, appName, err := s.withApp(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	accName := r.PathValue("acc")
	if err := validateAppName(accName); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	containerName := appName + "-" + accName
	dk := docker.NewClient(exec)
	if err := dk.Stop(r.Context(), containerName, 10); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

// --- Deploy handler ---

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		App    string `json:"app"`
		Image  string `json:"image"`
		Domain string `json:"domain"`
		Server string `json:"server"`
		Port   int    `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if err := validateAppName(body.App); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Image == "" {
		writeError(w, http.StatusBadRequest, "image is required")
		return
	}
	if body.Domain == "" {
		writeError(w, http.StatusBadRequest, "domain is required")
		return
	}
	if err := validateServerName(body.Server); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	exec, err := s.pool.Get(ctx, body.Server)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("cannot connect to %s: %v", body.Server, err))
		return
	}

	// Generate a short version hash from the image tag
	version := shortVersion(body.Image)

	containerPort := body.Port
	if containerPort == 0 {
		containerPort = 80
	}

	cfg := deploy.Config{
		App:           body.App,
		Image:         body.Image,
		Domain:        body.Domain,
		Version:       version,
		ContainerPort: containerPort,
	}

	deployer := deploy.NewDeployer(exec, io.Discard)
	if err := deployer.Deploy(ctx, cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "deployed",
		"app":     body.App,
		"version": version,
	})
}

// shortVersion extracts a short version identifier from a Docker image reference.
// e.g. "myapp:v1.2.3" → "v1-2-3", "nginx:latest" → "latest", "ghcr.io/user/app:abc123" → "abc123"
func shortVersion(image string) string {
	// Extract tag after the last colon (but not in the registry host part)
	parts := strings.Split(image, ":")
	tag := "latest"
	if len(parts) >= 2 {
		candidate := parts[len(parts)-1]
		// Make sure it's not a port number (like in ghcr.io:443/...)
		if !strings.Contains(candidate, "/") {
			tag = candidate
		}
	}
	// Sanitize: replace dots and special chars with hyphens
	tag = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + 32 // lowercase
		}
		return '-'
	}, tag)
	if len(tag) > 12 {
		tag = tag[:12]
	}
	if tag == "" {
		tag = "latest"
	}
	return tag
}

// --- Config handlers ---

func (s *Server) handleConfigListServers(w http.ResponseWriter, r *http.Request) {
	serversPath, err := config.DefaultServersPath()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}

	servers, err := config.ListServers(serversPath)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}

	// Re-serialize with json-friendly keys (config.Server only has yaml tags)
	result := make(map[string]any, len(servers))
	for name, srv := range servers {
		result[name] = map[string]any{
			"host": srv.Host,
			"user": srv.User,
			"role": srv.Role,
			"tags": srv.Tags,
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleConfigAddServer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		Host string `json:"host"`
		User string `json:"user"`
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if err := validateServerName(body.Name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Host == "" {
		writeError(w, http.StatusBadRequest, "host is required")
		return
	}

	serversPath, err := config.DefaultServersPath()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := config.AddServer(serversPath, body.Name, body.Host, body.User, body.Role, ""); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{"status": "created"})
}

func (s *Server) handleConfigEditServer(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validateServerName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var body struct {
		Host string `json:"host"`
		User string `json:"user"`
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if body.Host == "" {
		writeError(w, http.StatusBadRequest, "host is required")
		return
	}

	serversPath, err := config.DefaultServersPath()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := config.AddServer(serversPath, name, body.Host, body.User, body.Role, ""); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handleConfigDeleteServer(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validateServerName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	serversPath, err := config.DefaultServersPath()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := config.RemoveServer(serversPath, name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleGetNotifications(w http.ResponseWriter, r *http.Request) {
	cfg, err := loadNotificationConfig()
	if err != nil {
		writeJSON(w, http.StatusOK, notificationConfig{})
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) handleSetNotifications(w http.ResponseWriter, r *http.Request) {
	var cfg notificationConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if err := saveNotificationConfig(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

func (s *Server) handleListRegistries(w http.ResponseWriter, r *http.Request) {
	regs, err := loadRegistries()
	if err != nil || regs == nil {
		writeJSON(w, http.StatusOK, make([]registryEntry, 0))
		return
	}
	writeJSON(w, http.StatusOK, regs)
}

func (s *Server) handleAddRegistry(w http.ResponseWriter, r *http.Request) {
	var entry registryEntry
	if err := json.NewDecoder(r.Body).Decode(&entry); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if entry.Server == "" || entry.Username == "" {
		writeError(w, http.StatusBadRequest, "server and username are required")
		return
	}

	if err := addRegistry(entry); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{"status": "added"})
}

func (s *Server) handleDeleteRegistry(w http.ResponseWriter, r *http.Request) {
	server := r.PathValue("server")
	if server == "" {
		writeError(w, http.StatusBadRequest, "server name is required")
		return
	}

	if err := removeRegistry(server); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// --- Helpers ---

func parseIntSafe(s string) int {
	var n int
	fmt.Sscanf(s, "%d", &n)
	return n
}

// notificationConfig is stored at ~/.teploy/notifications.json
type notificationConfig struct {
	WebhookURL string `json:"webhook_url"`
}

func loadNotificationConfig() (notificationConfig, error) {
	return loadJSONFile[notificationConfig](teployConfigPath("notifications.json"))
}

func saveNotificationConfig(cfg notificationConfig) error {
	return saveJSONFile(teployConfigPath("notifications.json"), cfg)
}

// registryEntry represents a stored registry credential.
type registryEntry struct {
	Server   string `json:"server"`
	Username string `json:"username"`
	Password string `json:"password,omitempty"`
}

func loadRegistries() ([]registryEntry, error) {
	return loadJSONFile[[]registryEntry](teployConfigPath("registries.json"))
}

func addRegistry(entry registryEntry) error {
	regs, _ := loadRegistries()
	// Replace existing entry for same server
	found := false
	for i, r := range regs {
		if r.Server == entry.Server {
			regs[i] = entry
			found = true
			break
		}
	}
	if !found {
		regs = append(regs, entry)
	}
	return saveJSONFile(teployConfigPath("registries.json"), regs)
}

func removeRegistry(server string) error {
	regs, err := loadRegistries()
	if err != nil {
		return err
	}
	var filtered []registryEntry
	for _, r := range regs {
		if r.Server != server {
			filtered = append(filtered, r)
		}
	}
	return saveJSONFile(teployConfigPath("registries.json"), filtered)
}

// teployConfigPath returns ~/.teploy/<filename>.
func teployConfigPath(filename string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".teploy", filename)
}

// loadJSONFile reads and unmarshals a JSON file.
func loadJSONFile[T any](path string) (T, error) {
	var result T
	data, err := os.ReadFile(path)
	if err != nil {
		return result, err
	}
	err = json.Unmarshal(data, &result)
	return result, err
}

// saveJSONFile marshals and writes a JSON file, creating parent directories.
func saveJSONFile(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
