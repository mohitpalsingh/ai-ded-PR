package modelsvc

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"path/filepath"
	"strconv"
	"time"
)

type Server struct {
	Cmd  *exec.Cmd
	Port int
	URL  string
	Log  string
}

func StartServer(ctx context.Context, binary, modelPath string, port, ctxSize int, embedding bool) (*Server, error) {
	if binary == "" {
		binary = "llama-server"
	}
	if _, err := exec.LookPath(binary); err != nil {
		return nil, err
	}
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
	}
	if embedding {
		args = append(args, "--embedding")
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	// wait for health
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return &Server{Cmd: cmd, Port: port, URL: url, Log: logPath}, nil
			}
		}
		// also check /v1/models
		resp2, err2 := http.Get(url + "/v1/models")
		if err2 == nil {
			resp2.Body.Close()
			if resp2.StatusCode < 500 {
				return &Server{Cmd: cmd, Port: port, URL: url, Log: logPath}, nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	cmd.Process.Kill()
	return nil, fmt.Errorf("server did not become healthy; see %s", logPath)
}

func (s *Server) Stop() {
	if s == nil || s.Cmd == nil || s.Cmd.Process == nil {
		return
	}
	pid := s.Cmd.Process.Pid
	// kill the whole process group we created, then reap
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = s.Cmd.Process.Kill()
	_, _ = s.Cmd.Process.Wait()
}

func StartEmbeddingServer(ctx context.Context, binary, modelPath string, port, ctxSize int) (*Server, error) {
	return StartServer(ctx, binary, modelPath, port, ctxSize, true)
}
