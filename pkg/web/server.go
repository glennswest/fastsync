package web

import (
	"context"
	"crypto/tls"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"fastsync/pkg/mirror"
	"fastsync/pkg/release"
)

//go:embed templates/*
var templates embed.FS

// Config holds server configuration
type Config struct {
	DestRegistry string
	DestRepo     string
	Workers      int
	BlobWorkers  int
	MaxRetries   int
	Insecure     bool
}

// SyncJob tracks a sync operation
type SyncJob struct {
	ID           string    `json:"id"`
	Version      string    `json:"version"`
	Source       string    `json:"source"`
	Dest         string    `json:"dest"`
	DestRepo     string    `json:"dest_repo"`
	Status       string    `json:"status"`
	StartTime    time.Time `json:"start_time"`
	EndTime      time.Time `json:"end_time,omitempty"`
	Total        int       `json:"total"`
	Completed    int       `json:"completed"`
	Skipped      int       `json:"skipped"`
	Failed       int       `json:"failed"`
	CurrentImage string    `json:"current_image"`
	Error        string    `json:"error,omitempty"`
	Log          []string  `json:"log"`
}

// LogEntry represents a log entry
type LogEntry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
	Source  string    `json:"source"`
}

// Server handles the web GUI
type Server struct {
	Port     int
	Keychain authn.Keychain
	Config   Config

	mu         sync.RWMutex
	jobs       map[string]*SyncJob
	current    *SyncJob
	cancel     context.CancelFunc
	systemLogs []LogEntry
	tmpl       *template.Template
}

// NewServer creates a new web server
func NewServer(port int, keychain authn.Keychain, insecure bool, workers, blobWorkers int, destRepo string) *Server {
	return &Server{
		Port:     port,
		Keychain: keychain,
		Config: Config{
			DestRegistry: "",
			DestRepo:     destRepo,
			Workers:      workers,
			BlobWorkers:  blobWorkers,
			MaxRetries:   3,
			Insecure:     insecure,
		},
		jobs:       make(map[string]*SyncJob),
		systemLogs: []LogEntry{},
	}
}

// Start starts the web server
func (s *Server) Start() error {
	// Parse templates
	var err error
	s.tmpl, err = template.ParseFS(templates, "templates/*.html")
	if err != nil {
		return fmt.Errorf("parsing templates: %w", err)
	}

	mux := http.NewServeMux()

	// Main page
	mux.HandleFunc("/", s.handleIndex)

	// View routes (return HTML fragments for HTMX)
	mux.HandleFunc("/view/dashboard", s.handleViewDashboard)
	mux.HandleFunc("/view/explorer", s.handleViewExplorer)
	mux.HandleFunc("/view/openshift", s.handleViewOpenShift)
	mux.HandleFunc("/view/jobs", s.handleViewJobs)
	mux.HandleFunc("/view/logs", s.handleViewLogs)
	mux.HandleFunc("/view/settings", s.handleViewSettings)

	// API routes
	mux.HandleFunc("/api/sync", s.handleSync)
	mux.HandleFunc("/api/cancel", s.handleCancel)
	mux.HandleFunc("/api/status", s.handleStatus)

	// Registry API
	mux.HandleFunc("/api/registry/stats", s.handleRegistryStats)
	mux.HandleFunc("/api/registry/repos", s.handleRegistryRepos)
	mux.HandleFunc("/api/registry/tags", s.handleRegistryTags)

	// Jobs API
	mux.HandleFunc("/api/jobs", s.handleJobsList)
	mux.HandleFunc("/api/jobs/current", s.handleJobsCurrent)
	mux.HandleFunc("/api/jobs/current/detail", s.handleJobsCurrentDetail)
	mux.HandleFunc("/api/jobs/recent", s.handleJobsRecent)

	// Logs API
	mux.HandleFunc("/api/logs/sync", s.handleLogsSyncFragment)
	mux.HandleFunc("/api/logs/system", s.handleLogsSystem)
	mux.HandleFunc("/api/logs/errors", s.handleLogsErrors)
	mux.HandleFunc("/api/logs/clear", s.handleLogsClear)

	// OpenShift API
	mux.HandleFunc("/api/openshift/versions", s.handleOpenShiftVersions)

	// Settings API
	mux.HandleFunc("/api/settings/general", s.handleSettingsGeneral)
	mux.HandleFunc("/api/settings/performance", s.handleSettingsPerformance)
	mux.HandleFunc("/api/settings/pullsecret", s.handleSettingsPullSecret)
	mux.HandleFunc("/api/settings/auth-status", s.handleSettingsAuthStatus)
	mux.HandleFunc("/api/settings/registries", s.handleSettingsRegistries)

	s.logSystem("info", "FastSync web server starting on port %d", s.Port)

	addr := fmt.Sprintf(":%d", s.Port)
	fmt.Printf("Starting web GUI at http://localhost%s\n", addr)
	return http.ListenAndServe(addr, mux)
}

func (s *Server) logSystem(level, format string, args ...interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.systemLogs = append(s.systemLogs, LogEntry{
		Time:    time.Now(),
		Level:   level,
		Message: fmt.Sprintf(format, args...),
		Source:  "system",
	})
	// Keep only last 1000 entries
	if len(s.systemLogs) > 1000 {
		s.systemLogs = s.systemLogs[len(s.systemLogs)-1000:]
	}
}

// View handlers
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "" {
		http.NotFound(w, r)
		return
	}
	s.tmpl.ExecuteTemplate(w, "index.html", nil)
}

func (s *Server) handleViewDashboard(w http.ResponseWriter, r *http.Request) {
	s.tmpl.ExecuteTemplate(w, "dashboard.html", nil)
}

func (s *Server) handleViewExplorer(w http.ResponseWriter, r *http.Request) {
	s.tmpl.ExecuteTemplate(w, "explorer.html", nil)
}

func (s *Server) handleViewOpenShift(w http.ResponseWriter, r *http.Request) {
	s.tmpl.ExecuteTemplate(w, "openshift.html", map[string]interface{}{
		"DestRegistry": s.Config.DestRegistry,
	})
}

func (s *Server) handleViewJobs(w http.ResponseWriter, r *http.Request) {
	s.tmpl.ExecuteTemplate(w, "jobs.html", nil)
}

func (s *Server) handleViewLogs(w http.ResponseWriter, r *http.Request) {
	s.tmpl.ExecuteTemplate(w, "logs.html", nil)
}

func (s *Server) handleViewSettings(w http.ResponseWriter, r *http.Request) {
	s.tmpl.ExecuteTemplate(w, "settings.html", map[string]interface{}{
		"Config": s.Config,
	})
}

// Registry stats API
func (s *Server) handleRegistryStats(w http.ResponseWriter, r *http.Request) {
	// Get stats from destination registry
	s.mu.RLock()
	jobCount := len(s.jobs)
	var runningJob *SyncJob
	if s.current != nil && s.current.Status == "running" {
		runningJob = s.current
	}
	// Calculate total sync duration
	var totalDuration time.Duration
	var completedJobs int
	for _, job := range s.jobs {
		if job.Status == "completed" && !job.EndTime.IsZero() {
			totalDuration += job.EndTime.Sub(job.StartTime)
			completedJobs++
		}
	}
	s.mu.RUnlock()

	// Try to get repository count and estimate storage from local registry
	repoCount := 0
	tagCount := 0
	registry := s.Config.DestRegistry
	if registry == "" {
		registry = "fastregistry.gw.lo:5000"
	}
	repos, _ := s.listRepositories(registry)
	repoCount = len(repos)

	// Count tags across all repos (estimate of images)
	for _, repo := range repos {
		tags, _ := s.listTags(registry, repo)
		tagCount += len(tags)
	}

	status := "Healthy"
	statusClass := "success"
	registryStatusDetail := "Connected"
	if runningJob != nil {
		status = "Syncing"
		statusClass = "warning"
		registryStatusDetail = fmt.Sprintf("Syncing %s", runningJob.Version)
	}

	// Estimate storage (rough estimate: ~500MB per OpenShift component image average)
	storageEstimate := "-"
	if tagCount > 0 {
		estimatedGB := float64(tagCount) * 0.5 // 500MB average per tag
		if estimatedGB > 1000 {
			storageEstimate = fmt.Sprintf("~%.1f TB", estimatedGB/1000)
		} else {
			storageEstimate = fmt.Sprintf("~%.1f GB", estimatedGB)
		}
	}

	avgDuration := "-"
	if completedJobs > 0 {
		avg := totalDuration / time.Duration(completedJobs)
		avgDuration = avg.Round(time.Second).String()
	}

	html := fmt.Sprintf(`
		<div class="stat-card">
			<div class="stat-label">Registry Status</div>
			<div class="stat-value %s">%s</div>
			<div class="stat-subtitle">%s</div>
		</div>
		<div class="stat-card">
			<div class="stat-label">Repositories</div>
			<div class="stat-value">%d</div>
			<div class="stat-subtitle">%d total tags</div>
		</div>
		<div class="stat-card">
			<div class="stat-label">Sync Jobs</div>
			<div class="stat-value">%d</div>
			<div class="stat-subtitle">Avg: %s</div>
		</div>
		<div class="stat-card">
			<div class="stat-label">Est. Storage</div>
			<div class="stat-value">%s</div>
			<div class="stat-subtitle">Based on tag count</div>
		</div>
	`, statusClass, status, registryStatusDetail, repoCount, tagCount, jobCount, avgDuration, storageEstimate)

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(html))
}

// listTags gets tags for a repository
func (s *Server) listTags(registry, repo string) ([]string, error) {
	url := fmt.Sprintf("http://%s/v2/%s/tags/list", registry, repo)
	if !s.Config.Insecure {
		url = fmt.Sprintf("https://%s/v2/%s/tags/list", registry, repo)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	if s.Config.Insecure {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var tagList struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tagList); err != nil {
		return nil, err
	}

	return tagList.Tags, nil
}

func (s *Server) listRepositories(registry string) ([]string, error) {
	// Query the registry catalog
	url := fmt.Sprintf("http://%s/v2/_catalog", registry)
	if !s.Config.Insecure {
		url = fmt.Sprintf("https://%s/v2/_catalog", registry)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	if s.Config.Insecure {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var catalog struct {
		Repositories []string `json:"repositories"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return nil, err
	}

	return catalog.Repositories, nil
}

func (s *Server) handleRegistryRepos(w http.ResponseWriter, r *http.Request) {
	search := r.URL.Query().Get("search")
	registry := r.URL.Query().Get("registry")
	if registry == "" {
		registry = s.Config.DestRegistry
	}
	if registry == "" {
		registry = "fastregistry.gw.lo:5000"
	}

	repos, err := s.listRepositories(registry)
	if err != nil {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(fmt.Sprintf(`<div class="empty-state">Error: %s</div>`, err.Error())))
		return
	}

	// Filter by search
	if search != "" {
		var filtered []string
		for _, repo := range repos {
			if strings.Contains(strings.ToLower(repo), strings.ToLower(search)) {
				filtered = append(filtered, repo)
			}
		}
		repos = filtered
	}

	if len(repos) == 0 {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<div class="empty-state">No repositories found</div>`))
		return
	}

	var html strings.Builder
	for _, repo := range repos {
		html.WriteString(fmt.Sprintf(`
			<div class="tree-item" hx-get="/api/registry/tags?repo=%s&registry=%s" hx-target="#repo-details" hx-swap="innerHTML">
				<svg width="16" height="16" fill="none" viewBox="0 0 24 24" stroke="currentColor"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 7v10a2 2 0 002 2h14a2 2 0 002-2V9a2 2 0 00-2-2h-6l-2-2H5a2 2 0 00-2 2z"/></svg>
				%s
			</div>
		`, repo, registry, repo))
	}

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(html.String()))
}

func (s *Server) handleRegistryTags(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	registry := r.URL.Query().Get("registry")
	if repo == "" || registry == "" {
		http.Error(w, "repo and registry required", http.StatusBadRequest)
		return
	}

	// Get tags
	url := fmt.Sprintf("http://%s/v2/%s/tags/list", registry, repo)
	if !s.Config.Insecure {
		url = fmt.Sprintf("https://%s/v2/%s/tags/list", registry, repo)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	if s.Config.Insecure {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	resp, err := client.Get(url)
	if err != nil {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(fmt.Sprintf(`<div class="card-header"><div class="card-title">%s</div></div><div class="empty-state">Error: %s</div>`, repo, err.Error())))
		return
	}
	defer resp.Body.Close()

	var tagList struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tagList); err != nil {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(fmt.Sprintf(`<div class="empty-state">Error parsing tags: %s</div>`, err.Error())))
		return
	}

	var html strings.Builder
	html.WriteString(fmt.Sprintf(`<div class="card-header"><div class="card-title">%s</div><span class="badge badge-info">%d tags</span></div>`, repo, len(tagList.Tags)))

	if len(tagList.Tags) == 0 {
		html.WriteString(`<div class="empty-state">No tags found</div>`)
	} else {
		html.WriteString(`<div class="table-container"><table><thead><tr><th>Tag</th><th>Actions</th></tr></thead><tbody>`)
		for _, tag := range tagList.Tags {
			html.WriteString(fmt.Sprintf(`
				<tr>
					<td><code>%s</code></td>
					<td>
						<button class="btn btn-secondary btn-sm">Copy</button>
					</td>
				</tr>
			`, tag))
		}
		html.WriteString(`</tbody></table></div>`)
	}

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(html.String()))
}

// Sync handlers
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse form or JSON
	var version, source, dest, destRepo, pullSecretPath string
	var insecure bool

	contentType := r.Header.Get("Content-Type")
	if strings.Contains(contentType, "application/json") {
		var req struct {
			Version    string `json:"version"`
			Source     string `json:"source"`
			Dest       string `json:"dest"`
			DestRepo   string `json:"dest_repo"`
			PullSecret string `json:"pull_secret"`
			Insecure   bool   `json:"insecure"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		version = req.Version
		source = req.Source
		dest = req.Dest
		destRepo = req.DestRepo
		pullSecretPath = req.PullSecret
		insecure = req.Insecure
	} else {
		r.ParseForm()
		version = r.FormValue("version")
		source = r.FormValue("source")
		dest = r.FormValue("dest")
		destRepo = r.FormValue("dest_repo")
		pullSecretPath = r.FormValue("pull_secret")
		insecure = r.FormValue("insecure") == "true"
	}

	if version == "" || dest == "" {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<div class="card" style="background: rgba(239,68,68,0.1); border-color: var(--danger);"><div class="card-title" style="color: var(--danger);">Error: Version and destination are required</div></div>`))
		return
	}

	if source == "" {
		source = "quay.io/openshift-release-dev/ocp-release"
	}
	if destRepo == "" {
		destRepo = s.Config.DestRepo
	}
	if destRepo == "" {
		destRepo = "openshift/release"
	}

	s.mu.Lock()
	if s.current != nil && s.current.Status == "running" {
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<div class="card" style="background: rgba(234,179,8,0.1); border-color: var(--warning);"><div class="card-title" style="color: var(--warning);">A sync is already in progress</div></div>`))
		return
	}

	job := &SyncJob{
		ID:        fmt.Sprintf("%d", time.Now().UnixNano()),
		Version:   version,
		Source:    source,
		Dest:      dest,
		DestRepo:  destRepo,
		Status:    "running",
		StartTime: time.Now(),
		Log:       []string{},
	}
	s.jobs[job.ID] = job
	s.current = job
	s.mu.Unlock()

	// Load keychain
	keychain := s.Keychain
	if pullSecretPath != "" {
		if k, err := loadPullSecretKeychain(pullSecretPath); err == nil {
			keychain = k
		}
	}

	// Start sync in background
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	go s.runSync(ctx, job, keychain, insecure || s.Config.Insecure)

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(fmt.Sprintf(`
		<div class="card" style="background: rgba(34,197,94,0.1); border-color: var(--success);">
			<div class="card-title" style="color: var(--success);">Sync started for OpenShift %s</div>
			<p style="color: var(--text-secondary); margin-top: 8px;">Syncing from %s to %s/%s</p>
			<a class="btn btn-primary btn-sm" style="margin-top: 12px;" hx-get="/view/jobs" hx-target="#main-content">View Progress</a>
		</div>
	`, version, source, dest, destRepo)))
}

func loadPullSecretKeychain(path string) (authn.Keychain, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var ps struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(data, &ps); err != nil {
		return nil, err
	}

	auths := make(map[string]authn.AuthConfig)
	for registry, auth := range ps.Auths {
		if auth.Auth != "" {
			decoded, _ := base64.StdEncoding.DecodeString(auth.Auth)
			parts := strings.SplitN(string(decoded), ":", 2)
			if len(parts) == 2 {
				auths[registry] = authn.AuthConfig{
					Username: parts[0],
					Password: parts[1],
				}
			}
		}
	}
	return mirror.NewPullSecretKeychain(auths), nil
}

func (s *Server) runSync(ctx context.Context, job *SyncJob, keychain authn.Keychain, insecure bool) {
	defer func() {
		s.mu.Lock()
		job.EndTime = time.Now()
		if job.Status == "running" {
			job.Status = "completed"
		}
		s.mu.Unlock()
	}()

	s.addLog(job, fmt.Sprintf("Starting sync of OpenShift %s", job.Version))
	s.addLog(job, fmt.Sprintf("Source: %s", job.Source))
	s.addLog(job, fmt.Sprintf("Destination: %s/%s", job.Dest, job.DestRepo))

	// Fetch release info
	s.addLog(job, "Fetching release manifest...")
	rel, err := release.GetRelease(job.Source, job.Version, keychain, insecure)
	if err != nil {
		s.setError(job, fmt.Sprintf("Failed to fetch release: %v", err))
		return
	}
	s.addLog(job, fmt.Sprintf("Found %d images in release", len(rel.Components)))

	s.mu.Lock()
	job.Total = len(rel.Components) + 1
	s.mu.Unlock()

	// Create mirror engine
	engine := mirror.NewEngine(s.Config.Workers, s.Config.BlobWorkers, job.Dest, job.DestRepo, keychain, insecure)

	// Progress callback
	engine.OnResult = func(r mirror.Result) {
		s.mu.Lock()
		job.CurrentImage = r.Component
		job.Completed = int(engine.Progress.Completed)
		job.Skipped = int(engine.Progress.Skipped)
		job.Failed = int(engine.Progress.Failed)
		s.mu.Unlock()

		status := "OK"
		if r.Error != nil {
			status = fmt.Sprintf("FAILED: %v", r.Error)
		} else if r.Skipped {
			status = "SKIPPED (exists)"
		}
		s.addLog(job, fmt.Sprintf("[%d/%d] %s: %s", engine.Progress.Completed+1, engine.Progress.Total, r.Component, status))
	}

	// Run mirror
	s.addLog(job, "Mirroring images...")
	if err := engine.Mirror(ctx, rel); err != nil {
		if ctx.Err() != nil {
			s.setError(job, "Sync cancelled by user")
		} else {
			s.setError(job, fmt.Sprintf("Mirror failed: %v", err))
		}
		return
	}

	s.mu.Lock()
	job.Completed = int(engine.Progress.Completed)
	job.Skipped = int(engine.Progress.Skipped)
	job.Failed = int(engine.Progress.Failed)
	s.mu.Unlock()

	elapsed := time.Since(job.StartTime).Round(time.Second)
	s.addLog(job, "=== Complete ===")
	s.addLog(job, fmt.Sprintf("Total: %d images", engine.Progress.Total))
	s.addLog(job, fmt.Sprintf("Copied: %d", int(engine.Progress.Completed)-int(engine.Progress.Skipped)-int(engine.Progress.Failed)))
	s.addLog(job, fmt.Sprintf("Skipped: %d (already exist)", engine.Progress.Skipped))
	s.addLog(job, fmt.Sprintf("Failed: %d", engine.Progress.Failed))
	s.addLog(job, fmt.Sprintf("Duration: %s", elapsed))
}

func (s *Server) addLog(job *SyncJob, msg string) {
	s.mu.Lock()
	job.Log = append(job.Log, fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), msg))
	s.mu.Unlock()
}

func (s *Server) setError(job *SyncJob, msg string) {
	s.mu.Lock()
	job.Status = "failed"
	job.Error = msg
	s.mu.Unlock()
	s.addLog(job, "ERROR: "+msg)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "cancelled"})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	if s.current != nil {
		json.NewEncoder(w).Encode(s.current)
	} else {
		json.NewEncoder(w).Encode(map[string]string{"status": "idle"})
	}
}

// Jobs handlers
func (s *Server) handleJobsList(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	jobs := make([]*SyncJob, 0, len(s.jobs))
	for _, job := range s.jobs {
		jobs = append(jobs, job)
	}
	s.mu.RUnlock()

	// Sort by start time descending
	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].StartTime.After(jobs[j].StartTime)
	})

	if len(jobs) == 0 {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<div class="empty-state">No sync jobs yet</div>`))
		return
	}

	var html strings.Builder
	html.WriteString(`<table><thead><tr><th>Version</th><th>Status</th><th>Progress</th><th>Started</th><th>Duration</th></tr></thead><tbody>`)

	for _, job := range jobs {
		statusBadge := "badge-info"
		switch job.Status {
		case "completed":
			statusBadge = "badge-success"
		case "failed":
			statusBadge = "badge-danger"
		case "running":
			statusBadge = "badge-warning"
		}

		duration := "-"
		if !job.EndTime.IsZero() {
			duration = job.EndTime.Sub(job.StartTime).Round(time.Second).String()
		} else if job.Status == "running" {
			duration = time.Since(job.StartTime).Round(time.Second).String() + "..."
		}

		progress := fmt.Sprintf("%d/%d", job.Completed, job.Total)

		html.WriteString(fmt.Sprintf(`
			<tr>
				<td><strong>%s</strong></td>
				<td><span class="badge %s">%s</span></td>
				<td>%s</td>
				<td>%s</td>
				<td>%s</td>
			</tr>
		`, job.Version, statusBadge, job.Status, progress, job.StartTime.Format("Jan 2 15:04"), duration))
	}

	html.WriteString(`</tbody></table>`)
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(html.String()))
}

func (s *Server) handleJobsCurrent(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	job := s.current
	s.mu.RUnlock()

	if job == nil || job.Status != "running" {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<div class="card-header"><div class="card-title">No Active Sync</div></div><div class="empty-state">No sync operation is currently running</div>`))
		return
	}

	percent := 0
	if job.Total > 0 {
		percent = (job.Completed * 100) / job.Total
	}

	html := fmt.Sprintf(`
		<div class="card-header">
			<div class="card-title">Syncing OpenShift %s</div>
			<button class="btn btn-danger btn-sm" hx-post="/api/cancel" hx-swap="none">Cancel</button>
		</div>
		<div class="progress-container">
			<div class="progress-header">
				<span>%d / %d images</span>
				<span>%d%%</span>
			</div>
			<div class="progress-bar">
				<div class="progress-fill" style="width: %d%%"></div>
			</div>
		</div>
		<div style="display: grid; grid-template-columns: repeat(4, 1fr); gap: 12px; margin-top: 16px;">
			<div style="text-align: center;"><div style="font-size: 1.5rem; font-weight: 700;">%d</div><div style="font-size: 0.75rem; color: var(--text-muted);">Total</div></div>
			<div style="text-align: center;"><div style="font-size: 1.5rem; font-weight: 700; color: var(--success);">%d</div><div style="font-size: 0.75rem; color: var(--text-muted);">Completed</div></div>
			<div style="text-align: center;"><div style="font-size: 1.5rem; font-weight: 700; color: var(--warning);">%d</div><div style="font-size: 0.75rem; color: var(--text-muted);">Skipped</div></div>
			<div style="text-align: center;"><div style="font-size: 1.5rem; font-weight: 700; color: var(--danger);">%d</div><div style="font-size: 0.75rem; color: var(--text-muted);">Failed</div></div>
		</div>
		<div style="margin-top: 16px; padding: 12px; background: var(--bg-tertiary); border-radius: var(--radius); font-family: monospace; font-size: 0.85rem;">
			Current: %s
		</div>
	`, job.Version, job.Completed, job.Total, percent, percent, job.Total, job.Completed, job.Skipped, job.Failed, job.CurrentImage)

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(html))
}

func (s *Server) handleJobsCurrentDetail(w http.ResponseWriter, r *http.Request) {
	s.handleJobsCurrent(w, r)
}

func (s *Server) handleJobsRecent(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	jobs := make([]*SyncJob, 0, len(s.jobs))
	for _, job := range s.jobs {
		jobs = append(jobs, job)
	}
	s.mu.RUnlock()

	// Sort and limit to 5
	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].StartTime.After(jobs[j].StartTime)
	})
	if len(jobs) > 5 {
		jobs = jobs[:5]
	}

	if len(jobs) == 0 {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<div class="empty-state">No recent sync jobs</div>`))
		return
	}

	var html strings.Builder
	html.WriteString(`<table><thead><tr><th>Version</th><th>Status</th><th>Started</th></tr></thead><tbody>`)

	for _, job := range jobs {
		statusBadge := "badge-info"
		switch job.Status {
		case "completed":
			statusBadge = "badge-success"
		case "failed":
			statusBadge = "badge-danger"
		case "running":
			statusBadge = "badge-warning"
		}

		html.WriteString(fmt.Sprintf(`
			<tr>
				<td><strong>%s</strong></td>
				<td><span class="badge %s">%s</span></td>
				<td>%s</td>
			</tr>
		`, job.Version, statusBadge, job.Status, job.StartTime.Format("Jan 2 15:04")))
	}

	html.WriteString(`</tbody></table>`)
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(html.String()))
}

// Logs handlers
func (s *Server) handleLogsSyncFragment(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	var logs []string
	if s.current != nil {
		logs = s.current.Log
	}
	s.mu.RUnlock()

	if len(logs) == 0 {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<div class="log-entry">No sync logs available</div>`))
		return
	}

	var html strings.Builder
	for _, log := range logs {
		class := ""
		if strings.Contains(log, "FAILED") || strings.Contains(log, "ERROR") {
			class = "error"
		} else if strings.Contains(log, "OK") {
			class = "success"
		} else if strings.Contains(log, "SKIPPED") {
			class = "warning"
		}
		html.WriteString(fmt.Sprintf(`<div class="log-entry %s">%s</div>`, class, log))
	}

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(html.String()))
}

func (s *Server) handleLogsSystem(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	logs := s.systemLogs
	s.mu.RUnlock()

	if len(logs) == 0 {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<div class="log-entry">No system logs</div>`))
		return
	}

	var html strings.Builder
	for _, log := range logs {
		class := ""
		if log.Level == "error" {
			class = "error"
		} else if log.Level == "warn" {
			class = "warning"
		}
		html.WriteString(fmt.Sprintf(`<div class="log-entry %s"><span class="log-time">%s</span>%s</div>`, class, log.Time.Format("15:04:05"), log.Message))
	}

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(html.String()))
}

func (s *Server) handleLogsErrors(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	var errors []string
	if s.current != nil {
		for _, log := range s.current.Log {
			if strings.Contains(log, "FAILED") || strings.Contains(log, "ERROR") {
				errors = append(errors, log)
			}
		}
	}
	s.mu.RUnlock()

	if len(errors) == 0 {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<div class="log-entry success">No errors!</div>`))
		return
	}

	var html strings.Builder
	for _, log := range errors {
		html.WriteString(fmt.Sprintf(`<div class="log-entry error">%s</div>`, log))
	}

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(html.String()))
}

func (s *Server) handleLogsClear(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.systemLogs = []LogEntry{}
	if s.current != nil && s.current.Status != "running" {
		s.current.Log = []string{}
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "cleared"})
}

// OpenShift versions handler - queries quay.io for real versions
func (s *Server) handleOpenShiftVersions(w http.ResponseWriter, r *http.Request) {
	minor := r.URL.Query().Get("minor")

	// Query quay.io for actual tags
	repo := "quay.io/openshift-release-dev/ocp-release"
	ref, err := name.ParseReference(repo)
	if err != nil {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(fmt.Sprintf(`<div class="empty-state">Error parsing repo: %s</div>`, err.Error())))
		return
	}

	opts := []remote.Option{remote.WithAuthFromKeychain(s.Keychain)}
	tags, err := remote.List(ref.Context(), opts...)
	if err != nil {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(fmt.Sprintf(`<div class="empty-state">Error fetching tags: %s<br>Make sure pull secret is configured.</div>`, err.Error())))
		return
	}

	// Filter to x86_64 versions and sort
	var versions []string
	for _, tag := range tags {
		if !strings.HasSuffix(tag, "-x86_64") {
			continue
		}
		version := strings.TrimSuffix(tag, "-x86_64")
		// Only include semver-like versions (4.x.y)
		if !strings.HasPrefix(version, "4.") {
			continue
		}
		if minor != "" && !strings.HasPrefix(version, minor) {
			continue
		}
		versions = append(versions, version)
	}

	// Sort versions descending (newest first)
	sort.Slice(versions, func(i, j int) bool {
		return compareVersions(versions[i], versions[j]) > 0
	})

	// Limit to 50 versions
	if len(versions) > 50 {
		versions = versions[:50]
	}

	if len(versions) == 0 {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<div class="empty-state">No versions found matching the filter</div>`))
		return
	}

	var html strings.Builder
	for _, version := range versions {
		html.WriteString(fmt.Sprintf(`
			<div class="version-card">
				<div class="version-card-header">
					<span class="version-number">%s</span>
					<span class="badge badge-success">available</span>
				</div>
				<div class="version-meta">Architecture: x86_64</div>
				<button class="btn btn-primary btn-sm" style="margin-top: 12px; width: 100%%;"
					hx-post="/api/sync"
					hx-vals='{"version": "%s", "source": "quay.io/openshift-release-dev/ocp-release", "dest": "fastregistry.gw.lo:5000", "dest_repo": "openshift/release", "insecure": true}'
					hx-target="#sync-result">
					Sync This Version
				</button>
			</div>
		`, version, version))
	}

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(html.String()))
}

// compareVersions compares two semantic versions, returns >0 if a > b
func compareVersions(a, b string) int {
	partsA := strings.Split(a, ".")
	partsB := strings.Split(b, ".")
	for i := 0; i < len(partsA) && i < len(partsB); i++ {
		var numA, numB int
		fmt.Sscanf(partsA[i], "%d", &numA)
		fmt.Sscanf(partsB[i], "%d", &numB)
		if numA != numB {
			return numA - numB
		}
	}
	return len(partsA) - len(partsB)
}

// Settings handlers
func (s *Server) handleSettingsGeneral(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.ParseForm()
	s.mu.Lock()
	s.Config.DestRegistry = r.FormValue("dest_registry")
	s.Config.DestRepo = r.FormValue("dest_repo")
	s.Config.Insecure = r.FormValue("insecure") == "on"
	s.mu.Unlock()

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<div class="card" style="background: rgba(34,197,94,0.1); border-color: var(--success);"><div class="card-title" style="color: var(--success);">Settings saved successfully</div></div>`))
}

func (s *Server) handleSettingsPerformance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.ParseForm()
	s.mu.Lock()
	fmt.Sscanf(r.FormValue("workers"), "%d", &s.Config.Workers)
	fmt.Sscanf(r.FormValue("blob_workers"), "%d", &s.Config.BlobWorkers)
	fmt.Sscanf(r.FormValue("max_retries"), "%d", &s.Config.MaxRetries)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<div class="card" style="background: rgba(34,197,94,0.1); border-color: var(--success);"><div class="card-title" style="color: var(--success);">Performance settings saved</div></div>`))
}

func (s *Server) handleSettingsPullSecret(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var pullSecretData []byte

	// Check for file upload
	file, _, err := r.FormFile("pullsecret")
	if err == nil {
		defer file.Close()
		pullSecretData, _ = io.ReadAll(file)
	}

	// Check for pasted content
	if len(pullSecretData) == 0 {
		content := r.FormValue("pullsecret_content")
		if content != "" {
			pullSecretData = []byte(content)
		}
	}

	if len(pullSecretData) == 0 {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<div class="card" style="background: rgba(239,68,68,0.1); border-color: var(--danger);"><div class="card-title" style="color: var(--danger);">No pull secret provided</div></div>`))
		return
	}

	// Validate JSON
	var ps struct {
		Auths map[string]interface{} `json:"auths"`
	}
	if err := json.Unmarshal(pullSecretData, &ps); err != nil {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(fmt.Sprintf(`<div class="card" style="background: rgba(239,68,68,0.1); border-color: var(--danger);"><div class="card-title" style="color: var(--danger);">Invalid JSON: %s</div></div>`, err.Error())))
		return
	}

	// Save to file
	if err := os.WriteFile("/tmp/pull-secret.json", pullSecretData, 0600); err != nil {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(fmt.Sprintf(`<div class="card" style="background: rgba(239,68,68,0.1); border-color: var(--danger);"><div class="card-title" style="color: var(--danger);">Failed to save: %s</div></div>`, err.Error())))
		return
	}

	// Update keychain
	if k, err := loadPullSecretKeychain("/tmp/pull-secret.json"); err == nil {
		s.Keychain = k
	}

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(fmt.Sprintf(`<div class="card" style="background: rgba(34,197,94,0.1); border-color: var(--success);"><div class="card-title" style="color: var(--success);">Pull secret saved (%d registries configured)</div></div>`, len(ps.Auths))))
}

func (s *Server) handleSettingsAuthStatus(w http.ResponseWriter, r *http.Request) {
	// Check if we have a keychain configured
	hasAuth := s.Keychain != nil && s.Keychain != authn.DefaultKeychain

	// Try to check if pull secret file exists
	_, err := os.Stat("/tmp/pull-secret.json")
	hasFile := err == nil

	var html strings.Builder
	if hasAuth || hasFile {
		html.WriteString(`<div style="display: flex; align-items: center; gap: 12px;"><span class="badge badge-success">Configured</span><span>Pull secret is loaded</span></div>`)
	} else {
		html.WriteString(`<div style="display: flex; align-items: center; gap: 12px;"><span class="badge badge-warning">Not Configured</span><span>No pull secret loaded - required for Quay.io sync</span></div>`)
	}

	// Test quay.io connectivity
	if hasAuth {
		html.WriteString(`<div style="margin-top: 12px;">`)
		ref, _ := name.ParseReference("quay.io/openshift-release-dev/ocp-release:4.18.0-x86_64")
		_, err := remote.Head(ref, remote.WithAuthFromKeychain(s.Keychain))
		if err != nil {
			html.WriteString(fmt.Sprintf(`<span class="badge badge-danger">Quay.io: Failed</span> <span style="color: var(--text-muted); font-size: 0.8rem;">%s</span>`, err.Error()))
		} else {
			html.WriteString(`<span class="badge badge-success">Quay.io: Connected</span>`)
		}
		html.WriteString(`</div>`)
	}

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(html.String()))
}

func (s *Server) handleSettingsRegistries(w http.ResponseWriter, r *http.Request) {
	// Return configured registries
	html := `
		<tr>
			<td>quay.io</td>
			<td>Source</td>
			<td><span class="badge badge-info">HTTPS</span></td>
			<td><button class="btn btn-secondary btn-sm">Test</button></td>
		</tr>
		<tr>
			<td>fastregistry.gw.lo:5000</td>
			<td>Destination</td>
			<td><span class="badge badge-warning">HTTP</span></td>
			<td><button class="btn btn-secondary btn-sm">Test</button></td>
		</tr>
	`
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(html))
}
