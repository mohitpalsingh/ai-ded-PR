package modelsvc

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

type Server struct {
	Cmd  *exec.Cmd
	Port int
	URL  string
	Log  string

	logFile *os.File
	once    sync.Once
}

func StartServer(ctx context.Context, binary, modelPath string, port, ctxSize int, embedding bool) (*Server, error) {
	if binary == "" {
		binary = "llama-server"
	}
	if _, err := exec.LookPath(binary); err != nil {
		return nil, err
	}
	// Fail fast with a clear message if another instance already owns the port
	// (otherwise we could silently talk to, and later fail to kill, a stranger).
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("port %d already in use (another ai-ded-pr or llama-server running?)", port)
	}
	ln.Close()

	logPath := filepath.Join(os.TempDir(), fmt.Sprintf("ai-pr-review-%d.log", port))
	logFile, _ := os.Create(logPath)
	args := []string{
		"-m", modelPath,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"-c", strconv.Itoa(ctxSize),
		"--threads", "4",
		"--n-gpu-layers", "999",
		"-fa", "on",
		"-ctk", "q4_0",
		"-ctv", "q4_0",
		"--no-webui",
	}
	if embedding {
		args = append(args, "--embedding")
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		if logFile != nil {
			logFile.Close()
		}
		return nil, err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	srv := &Server{Cmd: cmd, Port: port, URL: url, Log: logPath, logFile: logFile}
	// wait for health
	hc := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		// bail out early if the process died (bad flag, bad model, OOM...)
		if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
			srv.Stop()
			return nil, fmt.Errorf("server exited during startup; see %s", logPath)
		}
		resp, err := hc.Get(url + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return srv, nil
			}
		}
		// also check /v1/models
		resp2, err2 := hc.Get(url + "/v1/models")
		if err2 == nil {
			resp2.Body.Close()
			if resp2.StatusCode < 500 {
				return srv, nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	srv.Stop()
	return nil, fmt.Errorf("server did not become healthy; see %s", logPath)
}

// Stop kills the whole process group we created, reaps the child, and closes
// the log handle. Safe to call multiple times and from multiple goroutines.
func (s *Server) Stop() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if s.Cmd != nil && s.Cmd.Process != nil {
			pid := s.Cmd.Process.Pid
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = s.Cmd.Process.Kill()
			_ = s.Cmd.Wait()
		}
		if s.logFile != nil {
			s.logFile.Close()
		}
	})
}

func StartEmbeddingServer(ctx context.Context, binary, modelPath string, port, ctxSize int) (*Server, error) {
	return StartServer(ctx, binary, modelPath, port, ctxSize, true)
}
